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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDeviceTypeFor(t *testing.T) {
	// The category decides which collector reports a device, so every model in
	// a realistic fleet has to land in the right one. The model is checked
	// before the family because an HS300 and an HS110 advertise the same
	// family and only one of them has outlets.
	tests := []struct {
		model  string
		family string
		want   DeviceType
	}{
		{model: "EP25(US)", family: "SMART.KASAPLUG", want: TypePlug},
		{model: "EP25", family: "SMART.KASAPLUG", want: TypePlug},
		{model: "HS110(US)", family: "IOT.SMARTPLUGSWITCH", want: TypePlug},
		{model: "KP125M(US)", family: "SMART.KASAPLUG", want: TypePlug},
		{model: "P110", family: "SMART.TAPOPLUG", want: TypePlug},

		{model: "HS300(US)", family: "IOT.SMARTPLUGSWITCH", want: TypeStrip},
		{model: "HS300(US) 2.0", family: "IOT.SMARTPLUGSWITCH", want: TypeStrip},
		{model: "KP303(UK)", family: "IOT.SMARTPLUGSWITCH", want: TypeStrip},
		{model: "EP40(US)", family: "IOT.SMARTPLUGSWITCH", want: TypeStrip},
		{model: "P300", family: "SMART.TAPOPLUG", want: TypeStrip},

		{model: "KL130(US)", family: "IOT.SMARTBULB", want: TypeBulb},
		{model: "LB130(US)", family: "IOT.SMARTBULB", want: TypeBulb},
		{model: "L530E", family: "SMART.TAPOBULB", want: TypeBulb},

		{model: "KL430(US)", family: "IOT.SMARTBULB", want: TypeLightStrip},
		{model: "L900-5", family: "SMART.TAPOBULB", want: TypeLightStrip},

		{model: "HS220(US)", family: "IOT.SMARTPLUGSWITCH", want: TypeDimmer},
		{model: "KS230(US)", family: "IOT.SMARTPLUGSWITCH", want: TypeDimmer},

		{model: "HS200(US)", family: "IOT.SMARTPLUGSWITCH", want: TypeWallSwitch},
		{model: "KS205(US)", family: "SMART.KASASWITCH", want: TypeWallSwitch},

		{model: "KH100(EU)", family: "IOT.SMARTPLUGSWITCH", want: TypeHub},
		{model: "H100", family: "SMART.TAPOHUB", want: TypeHub},

		// An unknown model falls back to the family, so new hardware still
		// lands somewhere sensible instead of vanishing.
		{model: "ZZ999", family: "SMART.TAPOHUB", want: TypeHub},
		{model: "ZZ999", family: "IOT.SMARTBULB", want: TypeBulb},
		{model: "ZZ999", family: "SMART.KASASWITCH", want: TypeWallSwitch},
		{model: "ZZ999", family: "SMART.KASAPLUG", want: TypePlug},
		{model: "ZZ999", family: "SOMETHING.ELSE", want: TypeUnknown},
		{model: "", family: "", want: TypeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.model+"/"+tt.family, func(t *testing.T) {
			if got := deviceTypeFor(tt.model, tt.family); got != tt.want {
				t.Fatalf("want %q, got %q", tt.want, got)
			}
		})
	}
}

// stubQuerier is a dialect that answers from a fixed reading, for testing the
// Device wrapper without a protocol underneath it.
type stubQuerier struct {
	reading *Reading
	err     error
	reads   int
	closed  int
}

func (s *stubQuerier) Read(context.Context) (*Reading, error) {
	s.reads++
	if s.err != nil {
		return nil, s.err
	}
	return s.reading, nil
}

func (s *stubQuerier) Close() { s.closed++ }

func TestDeviceReadRemembersAlias(t *testing.T) {
	querier := &stubQuerier{reading: &Reading{Alias: "Rack A", Model: "HS300(US)"}}
	device := &Device{host: "192.168.1.20", querier: querier}

	if want, have := "192.168.1.20", device.Alias(); want != have {
		t.Fatalf("want the address before the first read, have %q", have)
	}

	reading, err := device.Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := "192.168.1.20", reading.Host; want != have {
		t.Errorf("want host %q on the reading, have %q", want, have)
	}
	if want, have := "Rack A", device.Alias(); want != have {
		t.Errorf("want alias %q, have %q", want, have)
	}

	// The remembered name is what keeps a failed device reporting under the
	// name a dashboard already groups it by, rather than starting a new series
	// under the bare address at the moment it goes down.
	querier.err = errors.New("connection refused")
	device.querier = querier
	if _, err := device.Read(t.Context()); err == nil {
		t.Fatal("expected an error")
	}
	if want, have := "Rack A", device.Alias(); want != have {
		t.Errorf("want the alias kept after a failure, have %q", have)
	}
}

func TestDeviceRemembersItsType(t *testing.T) {
	// The category is remembered for the same reason as the name: a device
	// going down must not move between the buckets of the fleet inventory.
	querier := &stubQuerier{reading: &Reading{Alias: "Rack A", Type: TypeStrip}}
	device := &Device{host: "192.168.1.20", querier: querier}

	if want, have := TypeUnknown, device.Type(); want != have {
		t.Fatalf("want %q before the first read, have %q", want, have)
	}

	if _, err := device.Read(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := TypeStrip, device.Type(); want != have {
		t.Fatalf("want %q, have %q", want, have)
	}

	querier.err = errors.New("connection refused")
	device.querier = querier
	if _, err := device.Read(t.Context()); err == nil {
		t.Fatal("expected an error")
	}
	if want, have := TypeStrip, device.Type(); want != have {
		t.Fatalf("want the type kept after a failure, have %q", have)
	}
}

func TestDeviceReadDropsConnectionOnFailure(t *testing.T) {
	// A device that has rebooted holds a session the exporter no longer shares.
	// Dropping the connection is what makes the next scrape negotiate afresh
	// instead of failing for the life of the process.
	querier := &stubQuerier{err: errors.New("no route to host")}
	device := &Device{host: "192.168.1.20", querier: querier}

	if _, err := device.Read(t.Context()); err == nil {
		t.Fatal("expected an error")
	}
	if querier.closed != 1 {
		t.Errorf("want the connection closed once, got %d", querier.closed)
	}
	if device.querier != nil {
		t.Error("want the connection dropped")
	}
}

func TestDeviceCloseIsIdempotent(t *testing.T) {
	querier := &stubQuerier{reading: &Reading{}}
	device := &Device{host: "192.168.1.20", querier: querier}

	device.Close()
	device.Close()
	if querier.closed != 1 {
		t.Fatalf("want the connection closed once, got %d", querier.closed)
	}
}

func TestConnectIsLazy(t *testing.T) {
	// A device that is switched off at start-up must not hold the exporter up,
	// so nothing is negotiated until the first read.
	device := Connect("192.168.1.20", Credentials{}, time.Second)
	if device.querier != nil {
		t.Fatal("want no connection before the first read")
	}
	if want, have := "192.168.1.20", device.Host(); want != have {
		t.Fatalf("want host %q, have %q", want, have)
	}
}

func TestNegotiateUsesTheDiscoveredTransport(t *testing.T) {
	// Discovery already said what the device speaks, so exactly one attempt is
	// made rather than probing every transport in turn.
	_, address := newXORServer(t, func([]byte) []byte {
		return []byte(`{"system":{"get_sysinfo":{"model":"HS110(US)","alias":"Freezer","relay_state":1,"err_code":0}},"emeter":{"err_code":-1}}`)
	})
	host, port := splitHostPort(t, address)

	querier, err := connectAs(host, Credentials{}, time.Second, &discoveryResult{EncryptType: encryptXOR})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The fake device cannot bind port 9999, so the transport is pointed at the
	// port it did get.
	iot, ok := querier.(*iotQuerier)
	if !ok {
		t.Fatalf("want an IOT dialect, got %T", querier)
	}
	iot.transport.(*xorTransport).port = port

	reading, err := querier.Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := "Freezer", reading.Alias; want != have {
		t.Fatalf("want alias %q, have %q", want, have)
	}
}

func TestConnectAsChoosesTheRightDialect(t *testing.T) {
	tests := []struct {
		name  string
		found *discoveryResult
		want  string
	}{
		{name: "legacy", found: &discoveryResult{EncryptType: encryptXOR}, want: "*kasa.iotQuerier"},
		{name: "no scheme", found: &discoveryResult{}, want: "*kasa.iotQuerier"},
		{
			name:  "klap on an iot device",
			found: &discoveryResult{EncryptType: encryptKLAP, Family: "IOT.SMARTPLUGSWITCH"},
			want:  "*kasa.iotQuerier",
		},
		{
			name:  "klap on a smart device",
			found: &discoveryResult{EncryptType: encryptKLAP, Family: "SMART.KASAPLUG"},
			want:  "*kasa.smartQuerier",
		},
		{
			name:  "aes",
			found: &discoveryResult{EncryptType: encryptAES, Family: "SMART.TAPOPLUG"},
			want:  "*kasa.smartQuerier",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			querier, err := connectAs("192.168.1.1", Credentials{}, time.Second, tt.found)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := fmt.Sprintf("%T", querier); got != tt.want {
				t.Fatalf("want %s, got %s", tt.want, got)
			}
		})
	}

	if _, err := connectAs("192.168.1.1", Credentials{}, time.Second, &discoveryResult{EncryptType: "ROT13"}); err == nil {
		t.Fatal("want an error for an unknown encryption scheme")
	}
}

func TestNegotiateReportsEveryAttempt(t *testing.T) {
	// A device that answers nothing produces one error naming every transport
	// tried, rather than a stack of unrelated failures per scrape.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	_, err := negotiate(ctx, "127.0.0.1", Credentials{}, 200*time.Millisecond, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	message := strings.ToLower(err.Error())
	for _, want := range []string{"klap", "aes", "9999"} {
		if !strings.Contains(message, want) {
			t.Errorf("want the error to mention %q, got %q", want, err)
		}
	}
}

func TestProbeOrderTriesNewFirmwareFirst(t *testing.T) {
	// Newer firmware leaves port 9999 closed, so leading with the legacy
	// transport means the attempt that always fails is also the one everything
	// else waits behind.
	order := probeOrder()
	if len(order) == 0 {
		t.Fatal("want at least one candidate")
	}
	if want, have := encryptKLAP, order[0].EncryptType; want != have {
		t.Errorf("want %q first, have %q", want, have)
	}
	if want, have := encryptXOR, order[len(order)-1].EncryptType; want != have {
		t.Errorf("want %q last, have %q", want, have)
	}
	for _, candidate := range order {
		if candidate.describe() == "" {
			t.Errorf("candidate %+v has no description", candidate)
		}
	}
}
