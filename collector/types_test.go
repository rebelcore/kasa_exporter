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

package collector

import (
	"log/slog"
	"testing"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// result wraps a reading in the shape the registry hands to the collectors.
func result(reading *kasa.Reading) kasa.Result {
	return kasa.Result{Host: reading.Host, Alias: reading.Alias, Reading: reading}
}

func TestPlugCollector(t *testing.T) {
	withSnapshot(t, []kasa.Result{
		result(&kasa.Reading{
			Host: "192.168.1.50", Alias: "Freezer", Type: kasa.TypePlug,
			On: boolean(true),
			Energy: &kasa.Energy{
				PowerWatts: float(41), TotalKWh: float(4.2),
			},
		}),
		// A device of another kind must not be reported here, or two collectors
		// would emit the same series for it.
		result(&kasa.Reading{Host: "192.168.1.20", Alias: "Rack A", Type: kasa.TypeStrip}),
	}, nil)

	got := mustCollect(t, NewPlugCollector)

	wantMetric(t, got, "kasa_plug_on{alias=Freezer,host=192.168.1.50}", 1)
	wantMetric(t, got, "kasa_power_watts{alias=Freezer,host=192.168.1.50}", 41)
	wantMetric(t, got, "kasa_energy_kilowatt_hours_total{alias=Freezer,host=192.168.1.50}", 4.2)
	// An EP25 measures neither voltage nor current. Exporting zeros for them
	// would read as a plug on a dead circuit.
	wantNoMetric(t, got, "kasa_voltage_volts{alias=Freezer,host=192.168.1.50}")
	wantNoMetric(t, got, "kasa_current_amperes{alias=Freezer,host=192.168.1.50}")
	wantNoMetric(t, got, "kasa_plug_on{alias=Rack A,host=192.168.1.20}")
}

func TestStripCollector(t *testing.T) {
	withSnapshot(t, []kasa.Result{
		result(&kasa.Reading{
			Host: "192.168.1.20", Alias: "Rack A", Type: kasa.TypeStrip,
			Energy: &kasa.Energy{
				PowerWatts: float(198), VoltageVolts: float(120.1),
				CurrentAmperes: float(1.65), TotalKWh: float(4.51),
			},
			Sockets: []kasa.Socket{
				{
					ID: "1", Alias: "Switch", On: boolean(true), UptimeSeconds: float(3600),
					Energy: &kasa.Energy{
						PowerWatts: float(150), VoltageVolts: float(120.1),
						CurrentAmperes: float(1.25), TotalKWh: float(4.2),
					},
				},
				// Two outlets share the name "Empty"; the position is what
				// keeps them apart.
				{ID: "2", Alias: "Empty", On: boolean(false)},
				{ID: "3", Alias: "Empty", On: boolean(true), UptimeSeconds: float(120)},
			},
		}),
	}, nil)

	got := mustCollect(t, NewStripCollector)

	wantMetric(t, got, "kasa_strip_sockets{alias=Rack A,host=192.168.1.20}", 3)
	wantMetric(t, got, "kasa_power_watts{alias=Rack A,host=192.168.1.20}", 198)
	wantMetric(t, got, "kasa_voltage_volts{alias=Rack A,host=192.168.1.20}", 120.1)
	wantMetric(t, got, "kasa_current_amperes{alias=Rack A,host=192.168.1.20}", 1.65)
	wantMetric(t, got, "kasa_energy_kilowatt_hours_total{alias=Rack A,host=192.168.1.20}", 4.51)

	wantMetric(t, got, "kasa_strip_socket_on{alias=Rack A,host=192.168.1.20,socket=1,socket_alias=Switch}", 1)
	wantMetric(t, got, "kasa_strip_socket_on{alias=Rack A,host=192.168.1.20,socket=2,socket_alias=Empty}", 0)
	wantMetric(t, got, "kasa_strip_socket_on{alias=Rack A,host=192.168.1.20,socket=3,socket_alias=Empty}", 1)
	wantMetric(t, got, "kasa_strip_socket_uptime_seconds{alias=Rack A,host=192.168.1.20,socket=1,socket_alias=Switch}", 3600)
	wantMetric(t, got, "kasa_strip_socket_power_watts{alias=Rack A,host=192.168.1.20,socket=1,socket_alias=Switch}", 150)
	wantMetric(t, got, "kasa_strip_socket_current_amperes{alias=Rack A,host=192.168.1.20,socket=1,socket_alias=Switch}", 1.25)
	wantMetric(t, got, "kasa_strip_socket_energy_kilowatt_hours_total{alias=Rack A,host=192.168.1.20,socket=1,socket_alias=Switch}", 4.2)

	// An outlet that could not be read has no energy series for this scrape,
	// but its switch state is still known from sysinfo.
	wantNoMetric(t, got, "kasa_strip_socket_power_watts{alias=Rack A,host=192.168.1.20,socket=2,socket_alias=Empty}")

	// The strip's own power draw and its outlets' are separate metric families,
	// so adding up a deployment does not count the same electricity twice.
	if _, ok := got["kasa_power_watts{alias=Rack A,host=192.168.1.20,socket=1,socket_alias=Switch}"]; ok {
		t.Error("want per-outlet power under its own metric name")
	}
}

func TestBulbCollector(t *testing.T) {
	withSnapshot(t, []kasa.Result{
		result(&kasa.Reading{
			Host: "192.168.1.60", Alias: "Hall", Type: kasa.TypeBulb,
			On: boolean(true), Brightness: float(80), Hue: float(120),
			Saturation: float(65), ColorTempKelvin: float(0),
			Energy: &kasa.Energy{PowerWatts: float(8.5)},
		}),
	}, nil)

	got := mustCollect(t, NewBulbCollector)

	wantMetric(t, got, "kasa_bulb_on{alias=Hall,host=192.168.1.60}", 1)
	wantMetric(t, got, "kasa_bulb_brightness_percent{alias=Hall,host=192.168.1.60}", 80)
	wantMetric(t, got, "kasa_bulb_hue_degrees{alias=Hall,host=192.168.1.60}", 120)
	wantMetric(t, got, "kasa_bulb_saturation_percent{alias=Hall,host=192.168.1.60}", 65)
	wantMetric(t, got, "kasa_bulb_color_temperature_kelvin{alias=Hall,host=192.168.1.60}", 0)
	wantMetric(t, got, "kasa_power_watts{alias=Hall,host=192.168.1.60}", 8.5)
}

func TestLightStripCollector(t *testing.T) {
	withSnapshot(t, []kasa.Result{
		result(&kasa.Reading{
			Host: "192.168.1.61", Alias: "Desk", Type: kasa.TypeLightStrip,
			On: boolean(false), Brightness: float(45), LightStripLength: float(16),
		}),
	}, nil)

	got := mustCollect(t, NewLightStripCollector)

	wantMetric(t, got, "kasa_lightstrip_on{alias=Desk,host=192.168.1.61}", 0)
	// The settings reported while a strip is off are the ones it will return
	// to, so the series stays continuous overnight.
	wantMetric(t, got, "kasa_lightstrip_brightness_percent{alias=Desk,host=192.168.1.61}", 45)
	wantMetric(t, got, "kasa_lightstrip_length{alias=Desk,host=192.168.1.61}", 16)
}

func TestDimmerCollector(t *testing.T) {
	withSnapshot(t, []kasa.Result{
		result(&kasa.Reading{
			Host: "192.168.1.70", Alias: "Dining", Type: kasa.TypeDimmer,
			On: boolean(true), Brightness: float(35),
			// A dimmer has no colour, so hue must not be exported even when the
			// field happens to carry a value.
			Hue: float(0),
		}),
	}, nil)

	got := mustCollect(t, NewDimmerCollector)

	wantMetric(t, got, "kasa_dimmer_on{alias=Dining,host=192.168.1.70}", 1)
	wantMetric(t, got, "kasa_dimmer_brightness_percent{alias=Dining,host=192.168.1.70}", 35)
	wantNoMetric(t, got, "kasa_dimmer_hue_degrees{alias=Dining,host=192.168.1.70}")
}

func TestWallSwitchCollector(t *testing.T) {
	withSnapshot(t, []kasa.Result{
		result(&kasa.Reading{
			Host: "192.168.1.71", Alias: "Porch", Type: kasa.TypeWallSwitch,
			On: boolean(false),
		}),
	}, nil)

	got := mustCollect(t, NewWallSwitchCollector)

	wantMetric(t, got, "kasa_wallswitch_on{alias=Porch,host=192.168.1.71}", 0)
}

func TestHubCollector(t *testing.T) {
	withSnapshot(t, []kasa.Result{
		result(&kasa.Reading{
			Host: "192.168.1.80", Alias: "Hub", Type: kasa.TypeHub,
			Children: []kasa.Child{
				{
					ID: "S1", Alias: "Garage", Model: "T310", Category: "subg.trigger.temp-hmdt-sensor",
					Online: boolean(true), BatteryPercent: float(88), BatteryLow: boolean(false),
					TemperatureCelsius: float(21.5), HumidityPercent: float(47),
					RSSI: float(-62), SignalLevel: float(2),
				},
				{
					ID: "S2", Alias: "Freezer", Model: "T315",
					Online: boolean(false), BatteryLow: boolean(true),
				},
			},
		}),
	}, nil)

	got := mustCollect(t, NewHubCollector)

	wantMetric(t, got, "kasa_hub_children{alias=Hub,host=192.168.1.80}", 2)
	wantMetric(t, got, "kasa_hub_child_up{alias=Hub,child_alias=Garage,child_id=S1,host=192.168.1.80}", 1)
	wantMetric(t, got, "kasa_hub_child_up{alias=Hub,child_alias=Freezer,child_id=S2,host=192.168.1.80}", 0)
	wantMetric(t, got, "kasa_hub_child_battery_percent{alias=Hub,child_alias=Garage,child_id=S1,host=192.168.1.80}", 88)
	wantMetric(t, got, "kasa_hub_child_battery_low{alias=Hub,child_alias=Freezer,child_id=S2,host=192.168.1.80}", 1)
	wantMetric(t, got, "kasa_hub_child_temperature_celsius{alias=Hub,child_alias=Garage,child_id=S1,host=192.168.1.80}", 21.5)
	wantMetric(t, got, "kasa_hub_child_humidity_percent{alias=Hub,child_alias=Garage,child_id=S1,host=192.168.1.80}", 47)
	wantMetric(t, got, "kasa_hub_child_signal_strength_dbm{alias=Hub,child_alias=Garage,child_id=S1,host=192.168.1.80}", -62)
	wantMetric(t, got, "kasa_hub_child_info{alias=Hub,category=subg.trigger.temp-hmdt-sensor,"+
		"child_alias=Garage,child_id=S1,host=192.168.1.80,model=T310}", 1)

	// A sensor that reports no battery must not be exported as a flat battery.
	wantNoMetric(t, got, "kasa_hub_child_battery_percent{alias=Hub,child_alias=Freezer,child_id=S2,host=192.168.1.80}")
}

func TestCollectorsSucceedWithNoDeviceOfTheirKind(t *testing.T) {
	// Most fleets have no bulbs. Reporting five of eight collectors as failed
	// for a working deployment would make the success metric useless.
	withSnapshot(t, []kasa.Result{
		result(&kasa.Reading{Host: "192.168.1.50", Alias: "Freezer", Type: kasa.TypePlug}),
	}, nil)

	for name, factory := range map[string]func(*slog.Logger) (Collector, error){
		"bulb":       NewBulbCollector,
		"lightstrip": NewLightStripCollector,
		"dimmer":     NewDimmerCollector,
		"wallswitch": NewWallSwitchCollector,
		"hub":        NewHubCollector,
		"strip":      NewStripCollector,
	} {
		t.Run(name, func(t *testing.T) {
			c, err := factory(testLogger())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got, err := collectMetrics(t, c)
			if err != nil {
				t.Fatalf("want a successful scrape, got %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("want nothing emitted, got %v", sortedKeys(got))
			}
		})
	}
}

func TestCollectorsReportNoDataForAnEmptyFleet(t *testing.T) {
	withSnapshot(t, nil, nil)

	for name, factory := range map[string]func(*slog.Logger) (Collector, error){
		"plug":       NewPlugCollector,
		"strip":      NewStripCollector,
		"bulb":       NewBulbCollector,
		"lightstrip": NewLightStripCollector,
		"dimmer":     NewDimmerCollector,
		"wallswitch": NewWallSwitchCollector,
		"hub":        NewHubCollector,
	} {
		t.Run(name, func(t *testing.T) {
			c, err := factory(testLogger())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, err := collectMetrics(t, c); !IsNoDataError(err) {
				t.Fatalf("want ErrNoData, got %v", err)
			}
		})
	}
}

// mustCollect builds a collector and runs it, failing the test on any error.
func mustCollect(t *testing.T, factory func(*slog.Logger) (Collector, error)) map[string]float64 {
	t.Helper()

	c, err := factory(testLogger())
	if err != nil {
		t.Fatalf("building the collector: %v", err)
	}
	got, err := collectMetrics(t, c)
	if err != nil {
		t.Fatalf("collecting: %v", err)
	}
	return got
}
