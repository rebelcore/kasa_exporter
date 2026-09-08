// Copyright 2010 Rebel Media
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kasa

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// stubTransport answers from a table of canned replies, keyed by a substring of
// the request. It stands in for a device when the point of the test is how a
// reply is interpreted rather than how it was fetched.
type stubTransport struct {
	replies  map[string]string
	fallback string
	requests []string
	err      error
}

func (s *stubTransport) Query(_ context.Context, request []byte) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.requests = append(s.requests, string(request))
	for match, reply := range s.replies {
		if strings.Contains(string(request), match) {
			return []byte(reply), nil
		}
	}
	if s.fallback != "" {
		return []byte(s.fallback), nil
	}
	return nil, errors.New("no canned reply for " + string(request))
}

func (s *stubTransport) Close() {}

// hs300Sysinfo is an HS300's reply, trimmed to the fields the exporter reads.
// The strip reports no relay state and no meter of its own: everything metered
// lives on the children.
const hs300Sysinfo = `{"system":{"get_sysinfo":{
	"sw_ver":"1.0.13 Build 210629 Rel.174901",
	"hw_ver":"2.0",
	"model":"HS300(US)",
	"deviceId":"8006ABCDEF0123456789ABCDEF012345",
	"oemId":"5C9E6254BEBAED63B2B6102966D24C17",
	"alias":"Rack A",
	"mic_type":"IOT.SMARTPLUGSWITCH",
	"mac":"AA:BB:CC:DD:EE:FF",
	"rssi":-53,
	"led_off":0,
	"updating":0,
	"child_num":3,
	"children":[
		{"id":"8006ABCDEF0123456789ABCDEF01234500","state":1,"alias":"Switch","on_time":3600},
		{"id":"8006ABCDEF0123456789ABCDEF01234501","state":0,"alias":"Empty","on_time":0},
		{"id":"02","state":1,"alias":"Empty","on_time":120}
	],
	"err_code":0
}}}`

func TestIOTReadStrip(t *testing.T) {
	// Each outlet reports its own meter in milli-units, which is the newer
	// firmware's spelling; the strip's own figures have to be derived from them.
	transport := &stubTransport{
		replies: map[string]string{
			"get_sysinfo":    hs300Sysinfo,
			"ABCDEF01234500": `{"emeter":{"get_realtime":{"current_ma":1250,"voltage_mv":120100,"power_mw":150000,"total_wh":4200,"err_code":0}}}`,
			"ABCDEF01234501": `{"emeter":{"get_realtime":{"current_ma":0,"voltage_mv":120100,"power_mw":0,"total_wh":10,"err_code":0}}}`,
			"ABCDEF01234502": `{"emeter":{"get_realtime":{"current_ma":400,"voltage_mv":120100,"power_mw":48000,"total_wh":300,"err_code":0}}}`,
		},
	}

	reading, err := newIOTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want, have := TypeStrip, reading.Type; want != have {
		t.Fatalf("want type %q, have %q", want, have)
	}
	if want, have := "Rack A", reading.Alias; want != have {
		t.Errorf("want alias %q, have %q", want, have)
	}
	if want, have := "HS300(US)", reading.Model; want != have {
		t.Errorf("want model %q, have %q", want, have)
	}
	if reading.RSSI == nil || *reading.RSSI != -53 {
		t.Errorf("want rssi -53, have %v", reading.RSSI)
	}
	if reading.LEDOn == nil || !*reading.LEDOn {
		t.Errorf("want the LED reported as lit, have %v", reading.LEDOn)
	}

	if want, have := 3, len(reading.Sockets); want != have {
		t.Fatalf("want %d sockets, have %d", want, have)
	}

	// Outlets are numbered by position, not by name: two of these are called
	// "Empty" and would otherwise collapse into one series.
	for i, want := range []string{"1", "2", "3"} {
		if have := reading.Sockets[i].ID; want != have {
			t.Errorf("socket %d: want id %q, have %q", i, want, have)
		}
	}
	if want, have := "Switch", reading.Sockets[0].Alias; want != have {
		t.Errorf("want first socket alias %q, have %q", want, have)
	}
	if reading.Sockets[0].On == nil || !*reading.Sockets[0].On {
		t.Errorf("want the first socket on, have %v", reading.Sockets[0].On)
	}
	if reading.Sockets[1].On == nil || *reading.Sockets[1].On {
		t.Errorf("want the second socket off, have %v", reading.Sockets[1].On)
	}
	if reading.Sockets[0].UptimeSeconds == nil || *reading.Sockets[0].UptimeSeconds != 3600 {
		t.Errorf("want 3600s uptime, have %v", reading.Sockets[0].UptimeSeconds)
	}

	if got := reading.Sockets[0].Energy; got == nil || *got.PowerWatts != 150 {
		t.Errorf("want 150 W on the first socket, have %v", got)
	}
	if got := reading.Sockets[0].Energy; got == nil || *got.CurrentAmperes != 1.25 {
		// The Python collector cast readings to int for InfluxDB, which rounded
		// a sub-amp draw to zero; the reading here keeps its precision.
		t.Errorf("want 1.25 A on the first socket, have %v", got)
	}

	// The strip's own figures are the sum of its outlets, except voltage, which
	// is shared: summing three readings of 120 V would report 360 V.
	if reading.Energy == nil {
		t.Fatal("want derived energy for the strip")
	}
	if want, have := 198.0, *reading.Energy.PowerWatts; want != have {
		t.Errorf("want %g W for the strip, have %g", want, have)
	}
	if want, have := 1.65, *reading.Energy.CurrentAmperes; want != have {
		t.Errorf("want %g A for the strip, have %g", want, have)
	}
	if want, have := 120.1, *reading.Energy.VoltageVolts; want != have {
		t.Errorf("want %g V for the strip, have %g", want, have)
	}
	if want, have := 4.51, *reading.Energy.TotalKWh; want != have {
		t.Errorf("want %g kWh for the strip, have %g", want, have)
	}
}

func TestIOTStripAddressesShortChildIDs(t *testing.T) {
	// Some firmware reports an outlet's position alone; the device only answers
	// to the full identifier, so the parent's must be prepended.
	transport := &stubTransport{
		replies: map[string]string{
			"get_sysinfo": hs300Sysinfo,
		},
		fallback: `{"emeter":{"get_realtime":{"power_mw":1000,"err_code":0}}}`,
	}

	if _, err := newIOTQuerier(transport).Read(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantID := "8006ABCDEF0123456789ABCDEF01234502"
	for _, request := range transport.requests {
		if strings.Contains(request, wantID) {
			return
		}
	}
	t.Fatalf("want a request addressing %s, got %v", wantID, transport.requests)
}

func TestIOTStripSurvivesOneFailingSocket(t *testing.T) {
	// One unhealthy outlet must not cost the readings of the others. The two
	// outlets that answer report a running total as well as a power draw, so the
	// missing cumulative figure below is one that was suppressed rather than one
	// the outlets never reported.
	transport := &stubTransport{
		replies: map[string]string{
			"get_sysinfo":    hs300Sysinfo,
			"ABCDEF01234500": `{"emeter":{"get_realtime":{"power_mw":150000,"total_wh":4200,"err_code":0}}}`,
			"ABCDEF01234501": `{"emeter":{"err_code":-1,"err_msg":"module not support"}}`,
			"ABCDEF01234502": `{"emeter":{"get_realtime":{"power_mw":48000,"total_wh":300,"err_code":0}}}`,
		},
	}

	reading, err := newIOTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reading.Sockets[1].Energy != nil {
		t.Errorf("want no energy for the failing socket, have %v", reading.Sockets[1].Energy)
	}
	if want, have := 198.0, *reading.Energy.PowerWatts; want != have {
		t.Errorf("want %g W from the working sockets, have %g", want, have)
	}

	// Power is a gauge: an undercount for one scrape is cosmetic and corrects
	// itself on the next. The strip's cumulative total is a counter, and a sum
	// missing an outlet is lower than the one before it — Prometheus reads that
	// as a counter reset and adds the whole pre-reset total into the next
	// increase(), inventing hundreds of kWh from one partial scrape. So the
	// total is left out of the reading entirely until every outlet answers.
	if reading.Energy.TotalKWh != nil {
		t.Errorf("want no cumulative total while an outlet is unread, have %g kWh", *reading.Energy.TotalKWh)
	}
}

func TestIOTStripWithholdsTheTotalWhenAnOutletCannotBeRead(t *testing.T) {
	// The other way an outlet drops out of the sum: not a meter that answers
	// with an error code, but a reply that never arrives intact. It has to
	// suppress the cumulative total just the same.
	transport := &stubTransport{
		replies: map[string]string{
			"get_sysinfo":    hs300Sysinfo,
			"ABCDEF01234500": `{"emeter":{"get_realtime":{"power_mw":150000,"total_wh":4200,"err_code":0}}}`,
			"ABCDEF01234501": `truncated`,
			"ABCDEF01234502": `{"emeter":{"get_realtime":{"power_mw":48000,"total_wh":300,"err_code":0}}}`,
		},
	}

	reading, err := newIOTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := 198.0, *reading.Energy.PowerWatts; want != have {
		t.Errorf("want %g W from the working sockets, have %g", want, have)
	}
	if reading.Energy.TotalKWh != nil {
		t.Errorf("want no cumulative total while an outlet is unread, have %g kWh", *reading.Energy.TotalKWh)
	}
}

func TestIOTStripReportsTheTotalWhenEveryOutletAnswers(t *testing.T) {
	// The counterpart to the partial read: with nothing missing, the strip's
	// cumulative total is the sum of its outlets and is reported as usual.
	transport := &stubTransport{
		replies: map[string]string{
			"get_sysinfo":    hs300Sysinfo,
			"ABCDEF01234500": `{"emeter":{"get_realtime":{"power_mw":150000,"total_wh":4200,"err_code":0}}}`,
			"ABCDEF01234501": `{"emeter":{"get_realtime":{"power_mw":0,"total_wh":10,"err_code":0}}}`,
			"ABCDEF01234502": `{"emeter":{"get_realtime":{"power_mw":48000,"total_wh":300,"err_code":0}}}`,
		},
	}

	reading, err := newIOTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reading.Energy == nil || reading.Energy.TotalKWh == nil {
		t.Fatalf("want a cumulative total for the strip, have %v", reading.Energy)
	}
	if want, have := 4.51, *reading.Energy.TotalKWh; want != have {
		t.Errorf("want %g kWh for the strip, have %g", want, have)
	}
}

func TestIOTStripFailsWhenNoSocketAnswers(t *testing.T) {
	// A strip reporting nothing at all is a strip the operator needs to hear
	// about, so it is a failed scrape rather than a silent zero.
	transport := &stubTransport{
		replies:  map[string]string{"get_sysinfo": hs300Sysinfo},
		fallback: "",
	}

	if _, err := newIOTQuerier(transport).Read(t.Context()); err == nil {
		t.Fatal("expected an error when no socket could be read")
	}
}

func TestIOTReadPlug(t *testing.T) {
	// An older HS110 reports whole units where an HS300 reports milli-units,
	// and its total is already in kilowatt-hours.
	transport := &stubTransport{
		fallback: `{"system":{"get_sysinfo":{
			"sw_ver":"1.2.5","hw_ver":"1.0","type":"IOT.SMARTPLUGSWITCH","model":"HS110(US)",
			"mac":"AA:BB:CC:00:11:22","deviceId":"ABC123","alias":"Freezer",
			"relay_state":1,"on_time":7200,"rssi":-42,"led_off":1,"err_code":0
		}},"emeter":{"get_realtime":{"current":0.34,"voltage":121.4,"power":41.2,"total":88.5,"err_code":0}}}`,
	}

	reading, err := newIOTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want, have := TypePlug, reading.Type; want != have {
		t.Fatalf("want type %q, have %q", want, have)
	}
	if reading.On == nil || !*reading.On {
		t.Errorf("want the plug on, have %v", reading.On)
	}
	if reading.LEDOn == nil || *reading.LEDOn {
		// The firmware reports led_off, so the value is inverted.
		t.Errorf("want the LED reported as off, have %v", reading.LEDOn)
	}
	if reading.Energy == nil {
		t.Fatal("want energy readings")
	}
	if want, have := 41.2, *reading.Energy.PowerWatts; want != have {
		t.Errorf("want %g W, have %g", want, have)
	}
	if want, have := 88.5, *reading.Energy.TotalKWh; want != have {
		t.Errorf("want %g kWh, have %g", want, have)
	}

	// Sysinfo and the meter travel in one request, so a plug costs one round
	// trip per scrape.
	if want, have := 1, len(transport.requests); want != have {
		t.Errorf("want %d request, have %d: %v", want, have, transport.requests)
	}
}

func TestIOTPlugWithoutMeterStopsAsking(t *testing.T) {
	// An HS100 has no meter. Asking for one on every scrape for the life of the
	// process is pure waste, so the module is dropped after the first miss.
	transport := &stubTransport{
		fallback: `{"system":{"get_sysinfo":{"model":"HS100(US)","alias":"Lamp","relay_state":0,"err_code":0}},
			"emeter":{"err_code":-1,"err_msg":"module not support"}}`,
	}

	querier := newIOTQuerier(transport)
	if _, err := querier.Read(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if querier.deviceHasEmeter {
		t.Fatal("want the meter to be dropped after it did not answer")
	}

	before := len(transport.requests)
	if _, err := querier.Read(t.Context()); err != nil {
		t.Fatalf("unexpected error on the second read: %v", err)
	}
	if got := len(transport.requests) - before; got != 1 {
		t.Fatalf("want 1 request on the second read, got %d", got)
	}
	if strings.Contains(transport.requests[len(transport.requests)-1], "emeter") {
		t.Fatal("want no emeter module in the second request")
	}
}

func TestIOTBulbUsesItsOwnMeterModule(t *testing.T) {
	// A bulb keeps its readings under a different module name, and the device
	// does not say so. The alternate name is tried once and then remembered.
	transport := &stubTransport{
		replies: map[string]string{
			"get_sysinfo": `{"system":{"get_sysinfo":{
				"model":"KL130(US)","mic_type":"IOT.SMARTBULB","alias":"Hall","mic_mac":"AA:BB:CC:DD:EE:00",
				"light_state":{"on_off":1,"hue":120,"saturation":65,"color_temp":0,"brightness":80},
				"rssi":-60,"err_code":0
			}},"emeter":{"err_code":-1}}`,
			"smartlife.iot.common.emeter": `{"smartlife.iot.common.emeter":{"get_realtime":{"power_mw":8500,"total_wh":120,"err_code":0}}}`,
		},
	}

	querier := newIOTQuerier(transport)
	reading, err := querier.Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want, have := TypeBulb, reading.Type; want != have {
		t.Fatalf("want type %q, have %q", want, have)
	}
	if reading.Energy == nil || *reading.Energy.PowerWatts != 8.5 {
		t.Fatalf("want 8.5 W, have %v", reading.Energy)
	}
	if want, have := "AA:BB:CC:DD:EE:00", reading.MAC; want != have {
		// Bulbs report mic_mac where plugs report mac.
		t.Errorf("want mac %q, have %q", want, have)
	}
	if want, have := iotBulbEmeterModule, querier.emeterModule; want != have {
		t.Errorf("want the module remembered as %q, have %q", want, have)
	}

	if reading.Brightness == nil || *reading.Brightness != 80 {
		t.Errorf("want brightness 80, have %v", reading.Brightness)
	}
	if reading.Hue == nil || *reading.Hue != 120 {
		t.Errorf("want hue 120, have %v", reading.Hue)
	}
}

func TestIOTBulbOffReportsTheSettingsItWillReturnTo(t *testing.T) {
	// While a bulb is off its live settings read as zero. Reporting those would
	// make a dimmed bulb look reset every evening, so the values it will come
	// back to are reported instead and the "on" reading says it is not lit.
	transport := &stubTransport{
		fallback: `{"system":{"get_sysinfo":{
			"model":"KL130(US)","mic_type":"IOT.SMARTBULB","alias":"Hall",
			"light_state":{"on_off":0,"dft_on_state":{"hue":30,"saturation":50,"color_temp":2700,"brightness":45}},
			"err_code":0
		}},"emeter":{"err_code":-1}}`,
	}

	reading, err := newIOTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reading.On == nil || *reading.On {
		t.Fatalf("want the bulb reported as off, have %v", reading.On)
	}
	if reading.Brightness == nil || *reading.Brightness != 45 {
		t.Errorf("want brightness 45, have %v", reading.Brightness)
	}
	if reading.ColorTempKelvin == nil || *reading.ColorTempKelvin != 2700 {
		t.Errorf("want colour temperature 2700, have %v", reading.ColorTempKelvin)
	}
}

func TestIOTReadErrors(t *testing.T) {
	tests := []struct {
		name  string
		reply string
	}{
		{name: "no system module", reply: `{"emeter":{"get_realtime":{}}}`},
		{name: "sysinfo error code", reply: `{"system":{"get_sysinfo":{"err_code":-1}}}`},
		{name: "not json", reply: `not json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &stubTransport{fallback: tt.reply}
			if _, err := newIOTQuerier(transport).Read(t.Context()); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestIOTReadTransportError(t *testing.T) {
	transport := &stubTransport{err: errors.New("connection refused")}
	if _, err := newIOTQuerier(transport).Read(t.Context()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestIOTEmeterUnits(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		power    float64
		voltage  float64
		current  float64
		total    float64
		hasValue bool
	}{
		{
			name:     "milli units",
			raw:      `{"current_ma":1250,"voltage_mv":120100,"power_mw":150000,"total_wh":4200}`,
			power:    150,
			voltage:  120.1,
			current:  1.25,
			total:    4.2,
			hasValue: true,
		},
		{
			name:     "whole units",
			raw:      `{"current":0.5,"voltage":230.2,"power":115.1,"total":12.5}`,
			power:    115.1,
			voltage:  230.2,
			current:  0.5,
			total:    12.5,
			hasValue: true,
		},
		{
			name: "energy spelt as energy_wh",
			// kasa's own reader treats energy_wh as valid alongside total_wh.
			// Reading only the total pair leaves such a device with no energy
			// series at all, silently.
			raw:      `{"power_mw":1000,"energy_wh":2500}`,
			power:    1,
			total:    2.5,
			hasValue: true,
		},
		{name: "module error", raw: `{"err_code":-1}`},
		{name: "nothing usable", raw: `{"err_code":0}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw iotEmeter
			if err := json.Unmarshal([]byte(tt.raw), &raw); err != nil {
				t.Fatalf("decoding: %v", err)
			}

			got := raw.energy()
			if !tt.hasValue {
				if got != nil {
					t.Fatalf("want no reading, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("want a reading, got none")
			}
			if tt.power != 0 && *got.PowerWatts != tt.power {
				t.Errorf("want %g W, got %g", tt.power, *got.PowerWatts)
			}
			if tt.voltage != 0 && *got.VoltageVolts != tt.voltage {
				t.Errorf("want %g V, got %g", tt.voltage, *got.VoltageVolts)
			}
			if tt.current != 0 && *got.CurrentAmperes != tt.current {
				t.Errorf("want %g A, got %g", tt.current, *got.CurrentAmperes)
			}
			if tt.total != 0 && *got.TotalKWh != tt.total {
				t.Errorf("want %g kWh, got %g", tt.total, *got.TotalKWh)
			}
		})
	}
}
