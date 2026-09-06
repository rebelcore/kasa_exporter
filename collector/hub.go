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

//go:build !nohub

package collector

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// hubCollector reports smart hubs — a KH100, an H100 — and the sensors paired
// to them. The hub itself measures nothing; everything of interest is a
// property of its children.
type hubCollector struct {
	children    *prometheus.Desc
	childInfo   *prometheus.Desc
	childUp     *prometheus.Desc
	battery     *prometheus.Desc
	batteryLow  *prometheus.Desc
	temperature *prometheus.Desc
	humidity    *prometheus.Desc
	signal      *prometheus.Desc
	signalLevel *prometheus.Desc
	logger      *slog.Logger
}

// childLabels identify one sensor paired to a hub. The child's own identifier
// is the label that keeps series distinct, because two sensors of the same
// model in the same room are routinely given the same name.
var childLabels = append(append([]string{}, deviceLabels...), "child_id", "child_alias")

func init() {
	registerCollector("hub", defaultEnabled, NewHubCollector)
}

// NewHubCollector builds the hub collector and its metric descriptors.
func NewHubCollector(logger *slog.Logger) (Collector, error) {
	const subsystem = "hub"

	return &hubCollector{
		children: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "children"),
			"Number of devices paired to the hub.",
			deviceLabels, nil,
		),
		childInfo: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "child_info"),
			"Static information about a device paired to the hub. Always 1; read the labels.",
			append(append([]string{}, childLabels...), "model", "category"),
			nil,
		),
		childUp: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "child_up"),
			"Whether the hub reports a paired device as online (1) or offline (0).",
			childLabels, nil,
		),
		battery: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "child_battery_percent"),
			"Battery charge of a paired device, as a percentage.",
			childLabels, nil,
		),
		batteryLow: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "child_battery_low"),
			"Whether a paired device reports its battery as low (1 = yes, 0 = no).",
			childLabels, nil,
		),
		temperature: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "child_temperature_celsius"),
			// Sensors can be configured to report in Fahrenheit; those readings
			// are converted, so this metric is always Celsius.
			"Temperature a paired sensor reports, in degrees Celsius.",
			childLabels, nil,
		),
		humidity: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "child_humidity_percent"),
			"Relative humidity a paired sensor reports, as a percentage.",
			childLabels, nil,
		),
		signal: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "child_signal_strength_dbm"),
			"Signal strength between the hub and a paired device, in dBm.",
			childLabels, nil,
		),
		signalLevel: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "child_signal_level"),
			"Signal quality between the hub and a paired device, from 0 (worst) to 3 (best).",
			childLabels, nil,
		),
		logger: logger,
	}, nil
}

// Update emits each hub's child count and then one set of readings per paired
// sensor.
func (c *hubCollector) Update(ch chan<- prometheus.Metric) error {
	results, err := snapshot(c.logger)
	if err != nil {
		c.logger.Error("Failed to build the device registry", "err", err)
		return err
	}

	readings, anyDevices := readingsOfType(results, kasa.TypeHub)
	if !anyDevices {
		return ErrNoData
	}

	for _, reading := range readings {
		labels := labelsFor(reading)
		ch <- prometheus.MustNewConstMetric(c.children, prometheus.GaugeValue, float64(len(reading.Children)), labels...)

		for _, child := range reading.Children {
			childLabelValues := append(append([]string{}, labels...), child.ID, child.Alias)

			ch <- prometheus.MustNewConstMetric(
				c.childInfo, prometheus.GaugeValue, 1,
				append(append([]string{}, childLabelValues...), child.Model, child.Category)...,
			)
			emitGauge(ch, c.childUp, boolValue(child.Online), childLabelValues...)
			emitGauge(ch, c.battery, child.BatteryPercent, childLabelValues...)
			emitGauge(ch, c.batteryLow, boolValue(child.BatteryLow), childLabelValues...)
			emitGauge(ch, c.temperature, child.TemperatureCelsius, childLabelValues...)
			emitGauge(ch, c.humidity, child.HumidityPercent, childLabelValues...)
			emitGauge(ch, c.signal, child.RSSI, childLabelValues...)
			emitGauge(ch, c.signalLevel, child.SignalLevel, childLabelValues...)
		}
	}

	return nil
}
