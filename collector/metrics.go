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
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rebelcore/kasa_exporter/config"
	"github.com/rebelcore/kasa_exporter/kasa"
)

// deviceLabels identify a device on every series it appears in. The model,
// firmware and identifiers are deliberately not among them: they belong on
// kasa_device_info, where they can be joined against without multiplying the
// label set of every reading.
var deviceLabels = []string{"host", "alias"}

// socketLabels extend the device labels to one outlet of a power strip.
var socketLabels = append(append([]string{}, deviceLabels...), "socket", "socket_alias")

// snapshot returns the scrape's device readings.
//
// The first collector to ask triggers the round of device queries; the rest,
// running concurrently in the same scrape, wait on it and share the result. A
// background context is used because every operation underneath is already
// bounded by its own configured timeout, and a device that is slow to answer
// should fail on that timeout rather than be cut short mid-handshake.
//
// It is a var so tests can substitute a fixed set of readings; production code
// uses the real implementation assigned here.
var snapshot = func(logger *slog.Logger) ([]kasa.Result, error) {
	registry, err := config.Registry(logger)
	if err != nil {
		return nil, err
	}
	return registry.Snapshot(context.Background()), nil
}

// readingsOfType returns the readings for devices of one kind, together with
// whether the scrape saw any devices at all.
//
// The two are separate answers: no devices of this kind is a successful scrape
// with nothing to report — most fleets have no bulbs — whereas no devices at
// all means the exporter has nothing to say, which is what ErrNoData is for.
func readingsOfType(results []kasa.Result, deviceType kasa.DeviceType) (readings []*kasa.Reading, anyDevices bool) {
	for _, result := range results {
		if result.Reading == nil {
			// A device that failed this scrape is still a device: it is
			// reported as unreachable by the device collector rather than
			// counting as an empty fleet here.
			anyDevices = true
			continue
		}
		anyDevices = true
		if result.Reading.Type == deviceType {
			readings = append(readings, result.Reading)
		}
	}
	return readings, anyDevices
}

// labelsFor returns the label values identifying a device.
func labelsFor(reading *kasa.Reading) []string {
	return []string{reading.Host, aliasOf(reading)}
}

// aliasOf returns the device's name, falling back to its address so a device
// that has never reported a name still carries a stable label.
func aliasOf(reading *kasa.Reading) string {
	if reading.Alias != "" {
		return reading.Alias
	}
	return reading.Host
}

// energyMetrics is the one set of energy descriptors the exporter has.
//
// Energy is a single metric family across the whole fleet rather than one per
// device kind, so that summing the power a deployment draws is one query and
// not a union over every kind of hardware in it. Each device is reported by
// exactly one collector, so there is no risk of two of them emitting the same
// series.
type energyMetrics struct {
	power   *prometheus.Desc
	voltage *prometheus.Desc
	current *prometheus.Desc
	energy  *prometheus.Desc
}

// newEnergyMetrics builds the shared energy descriptors.
func newEnergyMetrics() energyMetrics {
	return energyMetrics{
		power: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "power_watts"),
			"Instantaneous power draw of the device in watts.",
			deviceLabels, nil,
		),
		voltage: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "voltage_volts"),
			"Line voltage measured by the device in volts.",
			deviceLabels, nil,
		),
		current: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "current_amperes"),
			"Current drawn through the device in amperes.",
			deviceLabels, nil,
		),
		energy: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "energy_kilowatt_hours_total"),
			"Cumulative energy the device has metered in kilowatt-hours.",
			deviceLabels, nil,
		),
	}
}

// emit sends whichever of the four readings the device reported. A quantity the
// hardware does not measure is left out rather than exported as a zero, which
// would read as a device drawing no current.
func (m energyMetrics) emit(ch chan<- prometheus.Metric, energy *kasa.Energy, labels ...string) {
	if energy == nil {
		return
	}
	emitGauge(ch, m.power, energy.PowerWatts, labels...)
	emitGauge(ch, m.voltage, energy.VoltageVolts, labels...)
	emitGauge(ch, m.current, energy.CurrentAmperes, labels...)
	emitCounter(ch, m.energy, energy.TotalKWh, labels...)
}

// lightMetrics are the descriptors shared by everything that emits light. Bulbs,
// light strips and dimmers each own their own set under their own metric names,
// so a dashboard can address one kind of hardware without matching the others.
type lightMetrics struct {
	on         *prometheus.Desc
	brightness *prometheus.Desc
	colorTemp  *prometheus.Desc
	hue        *prometheus.Desc
	saturation *prometheus.Desc
}

// newLightMetrics builds the light descriptors under one subsystem. Colour is
// omitted for hardware that has none, which is why withColor is a parameter
// rather than always true: a dimmer has a brightness but no hue.
func newLightMetrics(subsystem, noun string, withColor bool) lightMetrics {
	m := lightMetrics{
		on: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "on"),
			"Whether the "+noun+" is switched on (1) or off (0).",
			deviceLabels, nil,
		),
		brightness: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "brightness_percent"),
			"Brightness the "+noun+" is set to, as a percentage.",
			deviceLabels, nil,
		),
	}
	if !withColor {
		return m
	}

	m.colorTemp = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, subsystem, "color_temperature_kelvin"),
		"Colour temperature the "+noun+" is set to, in kelvin. Zero while it is showing a colour rather than white.",
		deviceLabels, nil,
	)
	m.hue = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, subsystem, "hue_degrees"),
		"Hue the "+noun+" is set to, in degrees.",
		deviceLabels, nil,
	)
	m.saturation = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, subsystem, "saturation_percent"),
		"Saturation the "+noun+" is set to, as a percentage.",
		deviceLabels, nil,
	)
	return m
}

// emit sends the light readings a device reported.
//
// While a light is off the settings reported are the ones it will return to
// when switched back on, so the series stay continuous rather than dropping out
// every evening; the "on" metric is what says whether it is currently lit.
func (m lightMetrics) emit(ch chan<- prometheus.Metric, reading *kasa.Reading) {
	labels := labelsFor(reading)
	emitGauge(ch, m.on, boolValue(reading.On), labels...)
	emitGauge(ch, m.brightness, reading.Brightness, labels...)
	emitGauge(ch, m.colorTemp, reading.ColorTempKelvin, labels...)
	emitGauge(ch, m.hue, reading.Hue, labels...)
	emitGauge(ch, m.saturation, reading.Saturation, labels...)
}

// emitGauge sends a gauge when the device reported the value, and nothing when
// it did not. A nil descriptor is skipped too, which is what lets lightMetrics
// leave out the colour metrics for hardware that has no colour.
func emitGauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value *float64, labels ...string) {
	if desc == nil || value == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, *value, labels...)
}

// emitCounter sends a counter under the same rules as emitGauge.
func emitCounter(ch chan<- prometheus.Metric, desc *prometheus.Desc, value *float64, labels ...string) {
	if desc == nil || value == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, *value, labels...)
}

// boolValue renders an optional boolean as the 1/0 gauge value Prometheus
// expects, keeping "the device did not report this" distinct from "false".
func boolValue(v *bool) *float64 {
	if v == nil {
		return nil
	}
	value := 0.0
	if *v {
		value = 1.0
	}
	return &value
}
