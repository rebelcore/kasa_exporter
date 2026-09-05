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
	"errors"
	"strings"
	"testing"
)

// ep25Batch is an EP25's reply to the batched request the exporter sends:
// device information alongside both energy methods. The name arrives
// base64-encoded, as all SMART firmware sends it.
const ep25Batch = `{"error_code":0,"result":{"responses":[
	{"method":"get_device_info","error_code":0,"result":{
		"device_id":"802E1234567890ABCDEF",
		"fw_ver":"1.0.4 Build 230329 Rel.150633",
		"hw_ver":"2.6",
		"type":"SMART.KASAPLUG",
		"model":"EP25",
		"mac":"AA-BB-CC-DD-EE-FF",
		"nickname":"UmFjayBB",
		"ssid":"bXluZXR3b3Jr",
		"device_on":true,
		"on_time":12345,
		"overheated":false,
		"rssi":-48,
		"signal_level":3
	}},
	{"method":"get_energy_usage","error_code":0,"result":{
		"today_runtime":300,"month_runtime":8000,
		"today_energy":150,"month_energy":4200,
		"current_power":41200
	}},
	{"method":"get_current_power","error_code":0,"result":{"current_power":41}}
]}}`

func TestSMARTReadPlug(t *testing.T) {
	transport := &stubTransport{fallback: ep25Batch}

	reading, err := newSMARTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want, have := TypePlug, reading.Type; want != have {
		t.Fatalf("want type %q, have %q", want, have)
	}
	if want, have := ProtocolSMART, reading.Protocol; want != have {
		t.Errorf("want protocol %q, have %q", want, have)
	}
	// "UmFjayBB" is base64; reporting it verbatim would label every EP25 with
	// gibberish.
	if want, have := "Rack A", reading.Alias; want != have {
		t.Errorf("want alias %q, have %q", want, have)
	}
	if want, have := "EP25", reading.Model; want != have {
		t.Errorf("want model %q, have %q", want, have)
	}
	if want, have := "2.6", reading.HardwareVersion; want != have {
		t.Errorf("want hardware version %q, have %q", want, have)
	}
	if reading.On == nil || !*reading.On {
		t.Errorf("want the plug on, have %v", reading.On)
	}
	if reading.Overheated == nil || *reading.Overheated {
		t.Errorf("want overheated false, have %v", reading.Overheated)
	}
	if reading.RSSI == nil || *reading.RSSI != -48 {
		t.Errorf("want rssi -48, have %v", reading.RSSI)
	}
	if reading.UptimeSeconds == nil || *reading.UptimeSeconds != 12345 {
		t.Errorf("want 12345s uptime, have %v", reading.UptimeSeconds)
	}

	if reading.Energy == nil {
		t.Fatal("want energy readings")
	}
	// get_current_power reports watts and get_energy_usage reports milliwatts.
	// The watt figure wins, so a 41 W draw is not reported as 41.2 kW.
	if want, have := 41.0, *reading.Energy.PowerWatts; want != have {
		t.Errorf("want %g W, have %g", want, have)
	}
	if want, have := 4.2, *reading.Energy.TotalKWh; want != have {
		t.Errorf("want %g kWh, have %g", want, have)
	}

	// Everything travels in one batched request, so an EP25 costs a single
	// round trip per scrape.
	if want, have := 1, len(transport.requests); want != have {
		t.Fatalf("want %d request, have %d: %v", want, have, transport.requests)
	}
	if !strings.Contains(transport.requests[0], "multipleRequest") {
		t.Errorf("want a batched request, got %s", transport.requests[0])
	}
}

func TestSMARTFallsBackToMilliwatts(t *testing.T) {
	// Firmware without get_current_power still reports power, in milliwatts.
	transport := &stubTransport{fallback: `{"error_code":0,"result":{"responses":[
		{"method":"get_device_info","error_code":0,"result":{"model":"P110","type":"SMART.TAPOPLUG","device_on":true}},
		{"method":"get_energy_usage","error_code":0,"result":{"current_power":41200,"month_energy":4200}},
		{"method":"get_current_power","error_code":-1001,"result":null}
	]}}`}

	querier := newSMARTQuerier(transport)
	reading, err := querier.Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := 41.2, *reading.Energy.PowerWatts; want != have {
		t.Errorf("want %g W, have %g", want, have)
	}
	// A method that failed once is not asked for again.
	if querier.wantsPower {
		t.Error("want get_current_power to be dropped after it failed")
	}
	if !querier.wantsEnergy {
		t.Error("want get_energy_usage kept after it answered")
	}
}

func TestSMARTDropsUnsupportedMethods(t *testing.T) {
	// A plug with no meter answers only get_device_info. Asking it for three
	// methods on every scrape for the life of the process is pure waste.
	transport := &stubTransport{fallback: `{"error_code":0,"result":{"responses":[
		{"method":"get_device_info","error_code":0,"result":{"model":"P100","type":"SMART.TAPOPLUG","device_on":false}}
	]}}`}

	querier := newSMARTQuerier(transport)
	if _, err := querier.Read(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if querier.wantsEnergy || querier.wantsPower || querier.wantsChildren {
		t.Fatal("want every unanswered method dropped")
	}

	if _, err := querier.Read(t.Context()); err != nil {
		t.Fatalf("unexpected error on the second read: %v", err)
	}
	second := transport.requests[1]
	for _, method := range []string{smartGetEnergyUsage, smartGetCurrentPower, smartGetChildDeviceList} {
		if strings.Contains(second, method) {
			t.Errorf("want %s dropped from the second request, got %s", method, second)
		}
	}
}

func TestSMARTReadStrip(t *testing.T) {
	// A P300 reports its outlets as child devices, each with its own name and
	// position.
	transport := &stubTransport{fallback: `{"error_code":0,"result":{"responses":[
		{"method":"get_device_info","error_code":0,"result":{
			"model":"P300","type":"SMART.TAPOPLUG","nickname":"U3RyaXA=","device_id":"ABC"
		}},
		{"method":"get_child_device_list","error_code":0,"result":{"child_device_list":[
			{"device_id":"ABC00","nickname":"TkFT","position":0,"device_on":true,"on_time":600},
			{"device_id":"ABC01","nickname":"U3dpdGNo","position":1,"device_on":false}
		]}}
	]}}`}

	reading, err := newSMARTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want, have := TypeStrip, reading.Type; want != have {
		t.Fatalf("want type %q, have %q", want, have)
	}
	if want, have := 2, len(reading.Sockets); want != have {
		t.Fatalf("want %d sockets, have %d", want, have)
	}
	// The firmware counts positions from zero; the exported outlet numbers
	// count from one, matching how the hardware is labelled.
	if want, have := "1", reading.Sockets[0].ID; want != have {
		t.Errorf("want socket id %q, have %q", want, have)
	}
	if want, have := "NAS", reading.Sockets[0].Alias; want != have {
		t.Errorf("want socket alias %q, have %q", want, have)
	}
	if reading.Sockets[0].UptimeSeconds == nil || *reading.Sockets[0].UptimeSeconds != 600 {
		t.Errorf("want 600s uptime, have %v", reading.Sockets[0].UptimeSeconds)
	}
	if reading.Sockets[1].On == nil || *reading.Sockets[1].On {
		t.Errorf("want the second socket off, have %v", reading.Sockets[1].On)
	}
}

func TestSMARTReadHub(t *testing.T) {
	transport := &stubTransport{fallback: `{"error_code":0,"result":{"responses":[
		{"method":"get_device_info","error_code":0,"result":{
			"model":"H100","type":"SMART.TAPOHUB","nickname":"SHVi","device_id":"HUB1"
		}},
		{"method":"get_child_device_list","error_code":0,"result":{"child_device_list":[
			{"device_id":"S1","nickname":"R2FyYWdl","model":"T310","category":"subg.trigger.temp-hmdt-sensor",
			 "status":"online","at_low_battery":false,"battery_percentage":88,
			 "current_temp":21.5,"current_humidity":47,"temp_unit":"celsius","rssi":-62,"signal_level":2},
			{"device_id":"S2","nickname":"RnJlZXplcg==","model":"T315","category":"subg.trigger.temp-hmdt-sensor",
			 "status":"offline","at_low_battery":true,"battery_percentage":9,
			 "current_temp":-4,"temp_unit":"fahrenheit"}
		]}}
	]}}`}

	reading, err := newSMARTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want, have := TypeHub, reading.Type; want != have {
		t.Fatalf("want type %q, have %q", want, have)
	}
	if len(reading.Sockets) != 0 {
		t.Errorf("want a hub's children reported as sensors, not outlets, have %d sockets", len(reading.Sockets))
	}
	if want, have := 2, len(reading.Children); want != have {
		t.Fatalf("want %d children, have %d", want, have)
	}

	first := reading.Children[0]
	if want, have := "Garage", first.Alias; want != have {
		t.Errorf("want child alias %q, have %q", want, have)
	}
	if first.Online == nil || !*first.Online {
		t.Errorf("want the first child online, have %v", first.Online)
	}
	if first.TemperatureCelsius == nil || *first.TemperatureCelsius != 21.5 {
		t.Errorf("want 21.5 C, have %v", first.TemperatureCelsius)
	}
	if first.BatteryPercent == nil || *first.BatteryPercent != 88 {
		t.Errorf("want 88%% battery, have %v", first.BatteryPercent)
	}

	second := reading.Children[1]
	if second.Online == nil || *second.Online {
		t.Errorf("want the second child offline, have %v", second.Online)
	}
	if second.BatteryLow == nil || !*second.BatteryLow {
		t.Errorf("want a low battery reported, have %v", second.BatteryLow)
	}
	// A sensor set to Fahrenheit must be converted, not exported as if the
	// number were already Celsius.
	if second.TemperatureCelsius == nil || *second.TemperatureCelsius != -20 {
		t.Errorf("want -20 C for -4 F, have %v", second.TemperatureCelsius)
	}
}

func TestSMARTRetriesWhenBatchOmitsDeviceInfo(t *testing.T) {
	// A little older firmware answers multipleRequest with an empty batch. The
	// device is still perfectly readable one method at a time.
	transport := &stubTransport{
		replies: map[string]string{
			"multipleRequest": `{"error_code":0,"result":{"responses":[]}}`,
			`"get_device_info"`: `{"error_code":0,"result":{
				"model":"HS200","type":"SMART.KASASWITCH","nickname":"UG9yY2g=","device_on":true
			}}`,
		},
	}

	reading, err := newSMARTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := "Porch", reading.Alias; want != have {
		t.Errorf("want alias %q, have %q", want, have)
	}
	if want, have := TypeWallSwitch, reading.Type; want != have {
		t.Errorf("want type %q, have %q", want, have)
	}
}

func TestSMARTReadErrors(t *testing.T) {
	tests := []struct {
		name      string
		transport *stubTransport
	}{
		{
			name:      "device error code",
			transport: &stubTransport{fallback: `{"error_code":-1301}`},
		},
		{
			name:      "not json",
			transport: &stubTransport{fallback: `not json`},
		},
		{
			name:      "batch is not an object",
			transport: &stubTransport{fallback: `{"error_code":0,"result":[]}`},
		},
		{
			name:      "transport failure",
			transport: &stubTransport{err: errors.New("connection reset")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newSMARTQuerier(tt.transport).Read(t.Context()); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestSMARTAlias(t *testing.T) {
	tests := []struct {
		name     string
		nickname string
		want     string
	}{
		{name: "empty", nickname: "", want: ""},
		{name: "base64", nickname: "UmFjayBB", want: "Rack A"},
		{name: "base64 utf-8", nickname: "5Y+w5omA", want: "台所"},
		// Not every firmware encodes the name, and a name with a space or
		// punctuation is not valid base64 to begin with.
		{name: "plain text", nickname: "Kitchen Lamp!", want: "Kitchen Lamp!"},
		// "Hall" is also a valid base64 string, decoding to three bytes of
		// binary. Decoding it would replace a good label with rubbish.
		{name: "plain text that is also base64", nickname: "Hall", want: "Hall"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := smartDeviceInfo{Nickname: tt.nickname}
			if have := info.alias(); have != tt.want {
				t.Fatalf("want %q, have %q", tt.want, have)
			}
		})
	}
}
