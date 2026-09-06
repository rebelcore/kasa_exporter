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

//go:build !nostrip

package collector

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// stripCollector reports multi-outlet power strips — an HS300, a KP303, a
// P300 — both as a whole and outlet by outlet.
//
// An HS300 has no meter of its own: each outlet measures itself, and the
// strip's figures are the sum of them. The two levels are reported as separate
// metric families so that adding up a deployment's power draw does not count
// the same electricity twice.
type stripCollector struct {
	on            *prometheus.Desc
	sockets       *prometheus.Desc
	socketOn      *prometheus.Desc
	socketUptime  *prometheus.Desc
	socketPower   *prometheus.Desc
	socketVoltage *prometheus.Desc
	socketCurrent *prometheus.Desc
	socketEnergy  *prometheus.Desc
	energy        energyMetrics
	logger        *slog.Logger
}

func init() {
	registerCollector("strip", defaultEnabled, NewStripCollector)
}

// NewStripCollector builds the strip collector and its metric descriptors.
func NewStripCollector(logger *slog.Logger) (Collector, error) {
	const subsystem = "strip"

	return &stripCollector{
		on: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "on"),
			"Whether the strip as a whole is switched on (1) or off (0). Not reported by strips that only switch per outlet.",
			deviceLabels, nil,
		),
		sockets: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "sockets"),
			"Number of outlets the strip has.",
			deviceLabels, nil,
		),
		socketOn: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "socket_on"),
			"Whether an individual outlet is switched on (1) or off (0).",
			socketLabels, nil,
		),
		socketUptime: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "socket_uptime_seconds"),
			"Seconds an individual outlet has been switched on.",
			socketLabels, nil,
		),
		socketPower: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "socket_power_watts"),
			"Instantaneous power drawn through one outlet, in watts.",
			socketLabels, nil,
		),
		socketVoltage: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "socket_voltage_volts"),
			"Line voltage measured at one outlet, in volts.",
			socketLabels, nil,
		),
		socketCurrent: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "socket_current_amperes"),
			"Current drawn through one outlet, in amperes.",
			socketLabels, nil,
		),
		socketEnergy: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "socket_energy_kilowatt_hours_total"),
			"Cumulative energy metered at one outlet, in kilowatt-hours.",
			socketLabels, nil,
		),
		energy: newEnergyMetrics(),
		logger: logger,
	}, nil
}

// Update emits each strip's totals and then its outlets.
func (c *stripCollector) Update(ch chan<- prometheus.Metric) error {
	results, err := snapshot(c.logger)
	if err != nil {
		c.logger.Error("Failed to build the device registry", "err", err)
		return err
	}

	readings, anyDevices := readingsOfType(results, kasa.TypeStrip)
	if !anyDevices {
		return ErrNoData
	}

	for _, reading := range readings {
		labels := labelsFor(reading)

		emitGauge(ch, c.on, boolValue(reading.On), labels...)
		ch <- prometheus.MustNewConstMetric(c.sockets, prometheus.GaugeValue, float64(len(reading.Sockets)), labels...)
		c.energy.emit(ch, reading.Energy, labels...)

		for _, socket := range reading.Sockets {
			// The outlet's position on the strip, not its name: unused outlets
			// share the name "Empty" and would collapse into a single series.
			socketLabelValues := append(append([]string{}, labels...), socket.ID, socket.Alias)

			emitGauge(ch, c.socketOn, boolValue(socket.On), socketLabelValues...)
			emitGauge(ch, c.socketUptime, socket.UptimeSeconds, socketLabelValues...)
			if socket.Energy == nil {
				continue
			}
			emitGauge(ch, c.socketPower, socket.Energy.PowerWatts, socketLabelValues...)
			emitGauge(ch, c.socketVoltage, socket.Energy.VoltageVolts, socketLabelValues...)
			emitGauge(ch, c.socketCurrent, socket.Energy.CurrentAmperes, socketLabelValues...)
			emitCounter(ch, c.socketEnergy, socket.Energy.TotalKWh, socketLabelValues...)
		}
	}

	return nil
}
