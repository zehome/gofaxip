// This file is part of the GOfax.IP project - https://github.com/gonicus/gofaxip
// Copyright (C) 2014 GONICUS GmbH, Germany - http://www.gonicus.de
//
// This program is free software; you can redistribute it and/or
// modify it under the terms of the GNU General Public License
// as published by the Free Software Foundation; version 2
// of the License.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program; if not, write to the Free Software
// Foundation, Inc., 51 Franklin Street, Fifth Floor, Boston, MA  02110-1301, USA.

package main

import (
	"fmt"
	"github.com/fiorix/go-eventsocket/eventsocket"
	"gofaxlib"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

type transmission struct {
	faxjob FaxJob
	conn   *eventsocket.Connection

	pageChan   chan *gofaxlib.PageResult
	errorChan  chan FaxError
	resultChan chan *gofaxlib.FaxResult

	sessionlog gofaxlib.SessionLogger
}

func Transmit(faxjob FaxJob, sessionlog gofaxlib.SessionLogger) *transmission {
	t := &transmission{
		faxjob:     faxjob,
		pageChan:   make(chan *gofaxlib.PageResult),
		errorChan:  make(chan FaxError),
		resultChan: make(chan *gofaxlib.FaxResult),
		sessionlog: sessionlog,
	}
	go t.start()
	return t
}

func (t *transmission) PageSent() <-chan *gofaxlib.PageResult {
	return t.pageChan
}

func (t *transmission) Errors() <-chan FaxError {
	return t.errorChan
}

func (t *transmission) Result() <-chan *gofaxlib.FaxResult {
	return t.resultChan
}

// Connect to FreeSWITCH and originate a txfax
func (t *transmission) start() {

	if t.faxjob.Number == "" {
		t.errorChan <- NewFaxError("Number to dial is empty", false)
		return
	}

	if _, err := os.Stat(t.faxjob.Filename); err != nil {
		t.errorChan <- NewFaxError(err.Error(), false)
		return
	}

	var err error
	t.conn, err = eventsocket.Dial(gofaxlib.Config.Freeswitch.Socket, gofaxlib.Config.Freeswitch.Password)
	if err != nil {
		t.errorChan <- NewFaxError(err.Error(), true)
		return
	}
	defer t.conn.Close()

	// Enable event filter and events
	_, err = t.conn.Send(fmt.Sprintf("filter Unique-ID %v", t.faxjob.UUID))
	if err != nil {
		t.errorChan <- NewFaxError(err.Error(), true)
		return
	}
	_, err = t.conn.Send("event plain CHANNEL_CALLSTATE CUSTOM spandsp::txfaxnegociateresult spandsp::txfaxpageresult spandsp::txfaxresult")
	if err != nil {
		t.errorChan <- NewFaxError(err.Error(), true)
		return
	}

	// Assemble dialstring
	ds_variables_map := map[string]string{
		"ignore_early_media":           "true",
		"origination_uuid":             t.faxjob.UUID.String(),
		"origination_caller_id_number": t.faxjob.Cidnum,
		"origination_caller_id_name":   t.faxjob.Cidname,
		"fax_ident":                    t.faxjob.Ident,
		"fax_header":                   t.faxjob.Header,
		"fax_use_ecm":                  strconv.FormatBool(t.faxjob.UseECM),
		"fax_verbose":                  strconv.FormatBool(gofaxlib.Config.Freeswitch.Verbose),
	}

	ds_variables_pairs := make([]string, len(ds_variables_map))
	i := 0
	for k, v := range ds_variables_map {
		ds_variables_pairs[i] = fmt.Sprintf("%v='%v'", k, v)
		i++
	}
	ds_variables := strings.Join(ds_variables_pairs, ",")

	// Try gateways in configured order
	ds_gateways_strings := make([]string, len(gofaxlib.Config.Freeswitch.Gateway))
	for i, gw := range gofaxlib.Config.Freeswitch.Gateway {
		ds_gateways_strings[i] = fmt.Sprintf("sofia/gateway/%v/%v", gw, t.faxjob.Number)
	}
	ds_gateways := strings.Join(ds_gateways_strings, "|")

	dialstring := fmt.Sprintf("{%v}%v", ds_variables, ds_gateways)
	//t.sessionlog.Log(fmt.Sprintf("%v Dialstring: %v", faxjob.UUID, dialstring))

	// Originate call
	t.sessionlog.Log("Originating channel to", t.faxjob.Number)
	_, err = t.conn.Send(fmt.Sprintf("api originate %v, &txfax(%v)", dialstring, t.faxjob.Filename))
	if err != nil {
		t.conn.Send(fmt.Sprintf("uuid_dump %v", t.faxjob.UUID))
		hangupcause := strings.TrimSpace(err.Error())
		t.sessionlog.Log("Originate failed with hangup cause", hangupcause)
		t.errorChan <- NewFaxError(hangupcause, true)
		return
	}
	t.sessionlog.Log("Originate successful")

	result := gofaxlib.NewFaxResult(t.faxjob.UUID, t.sessionlog)

	es := gofaxlib.NewEventStream(t.conn)
	var pages uint

	// Listen for system signals to be able to kill the channel
	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case ev := <-es.Events():
			result.AddEvent(ev)
			if result.Hangupcause != "" {
				t.resultChan <- result
				return
			}
			if ev.Get("Event-Subclass") == "spandsp::txfaxnegociateresult" {
				t.resultChan <- result
			} else if result.TransferredPages != pages {
				pages = result.TransferredPages
				t.pageChan <- &result.PageResults[pages-1]
			}
		case err := <-es.Errors():
			t.errorChan <- NewFaxError(err.Error(), true)
			return
		case kill := <-sigchan:
			t.sessionlog.Log(fmt.Sprintf("%v Received signal %v, destroying channel", t.faxjob.UUID, kill))
			t.conn.Send(fmt.Sprintf("api uuid_kill %v", t.faxjob.UUID))
			os.Remove(t.faxjob.Filename)
			t.errorChan <- NewFaxError(fmt.Sprintf("Killed by signal %v", kill), false)
		}
	}

}
