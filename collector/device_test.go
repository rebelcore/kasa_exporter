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

//go:build !nodevice

package collector

import (
	"errors"
	"testing"

	"github.com/rebelcore/kasa_exporter/kasa"
)

func TestDeviceCollector(t *testing.T) {
	withSnapshot(t, []kasa.Result{
		{
			Host:  "192.168.1.20",
			Alias: "Rack A",
			Reading: &kasa.Reading{
				Host: "192.168.1.20", Alias: "Rack A", Model: "HS300(US)",
				DeviceID: "8006AB", HardwareVersion: "2.0", FirmwareVersion: "1.0.13",
				MAC: "AA:BB:CC:DD:EE:FF", Type: kasa.TypeStrip, Protocol: kasa.ProtocolIOT,
				RSSI: float(-53), UptimeSeconds: float(3600),
				LEDOn: boolean(true), Updating: boolean(false),
			},
		},
		{
			Host:  "192.168.1.50",
			Alias: "Freezer",
			Reading: &kasa.Reading{
				Host: "192.168.1.50", Alias: "Freezer", Model: "EP25",
				Type: kasa.TypePlug, Protocol: kasa.ProtocolSMART,
				Overheated: boolean(false), SignalLevel: float(3),
			},
		},
	}, nil)

	c, err := NewDeviceCollector(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := collectMetrics(t, c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantMetric(t, got, "kasa_device_up{alias=Rack A,host=192.168.1.20}", 1)
	wantMetric(t, got, "kasa_device_up{alias=Freezer,host=192.168.1.50}", 1)
	wantMetric(t, got, "kasa_device_signal_strength_dbm{alias=Rack A,host=192.168.1.20}", -53)
	wantMetric(t, got, "kasa_device_uptime_seconds{alias=Rack A,host=192.168.1.20}", 3600)
	wantMetric(t, got, "kasa_device_led_on{alias=Rack A,host=192.168.1.20}", 1)
	wantMetric(t, got, "kasa_device_updating{alias=Rack A,host=192.168.1.20}", 0)
	wantMetric(t, got, "kasa_device_overheated{alias=Freezer,host=192.168.1.50}", 0)
	wantMetric(t, got, "kasa_device_signal_level{alias=Freezer,host=192.168.1.50}", 3)

	// The model, versions and identifiers live on the info metric rather than
	// on every reading, so a dashboard joins against them instead of paying for
	// them on each series.
	wantMetric(t, got, "kasa_device_info{alias=Rack A,device_id=8006AB,firmware_version=1.0.13,"+
		"hardware_version=2.0,host=192.168.1.20,mac=AA:BB:CC:DD:EE:FF,model=HS300(US),protocol=iot,type=strip}", 1)

	wantMetric(t, got, "kasa_devices{type=strip}", 1)
	wantMetric(t, got, "kasa_devices{type=plug}", 1)

	// A device that reported nothing this scrape has no readings to emit.
	wantNoMetric(t, got, "kasa_device_signal_strength_dbm{alias=Freezer,host=192.168.1.50}")
}

func TestDeviceCollectorReportsUnreachableDevices(t *testing.T) {
	// A device that fails is reported at 0 under its last known name. Dropping
	// it from the output instead would make a failure indistinguishable from a
	// device that was never configured, and so impossible to alert on.
	withSnapshot(t, []kasa.Result{{
		Host:  "192.168.1.21",
		Alias: "Rack B",
		Type:  kasa.TypeStrip,
		Err:   errors.New("connection refused"),
	}}, nil)

	c, err := NewDeviceCollector(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := collectMetrics(t, c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantMetric(t, got, "kasa_device_up{alias=Rack B,host=192.168.1.21}", 0)
	wantNoMetric(t, got, "kasa_device_info{alias=Rack B,host=192.168.1.21}")

	// Counted under the category it was last read as, so a device going down
	// does not move between buckets and make the inventory flap.
	wantMetric(t, got, "kasa_devices{type=strip}", 1)
	wantNoMetric(t, got, "kasa_devices{type=unknown}")
}

func TestDeviceCollectorFallsBackToTheAddress(t *testing.T) {
	// A device that has never reported a name still needs a stable label.
	withSnapshot(t, []kasa.Result{{
		Host:    "192.168.1.22",
		Reading: &kasa.Reading{Host: "192.168.1.22", Type: kasa.TypeUnknown},
	}}, nil)

	c, err := NewDeviceCollector(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := collectMetrics(t, c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantMetric(t, got, "kasa_device_up{alias=192.168.1.22,host=192.168.1.22}", 1)
}

func TestDeviceCollectorNoDevices(t *testing.T) {
	withSnapshot(t, nil, nil)

	c, err := NewDeviceCollector(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := collectMetrics(t, c); !IsNoDataError(err) {
		t.Fatalf("want ErrNoData for an empty fleet, got %v", err)
	}
}

func TestDeviceCollectorRegistryFailure(t *testing.T) {
	withSnapshot(t, nil, errors.New("no devices to poll"))

	c, err := NewDeviceCollector(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := collectMetrics(t, c); err == nil {
		t.Fatal("want the configuration error surfaced as a failed scrape")
	}
}
