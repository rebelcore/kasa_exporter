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

//go:build !nowallswitch

package collector

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// wallSwitchCollector reports non-dimming wall switches — an HS200, a KS200.
type wallSwitchCollector struct {
	on     *prometheus.Desc
	energy energyMetrics
	logger *slog.Logger
}

func init() {
	registerCollector("wallswitch", defaultEnabled, NewWallSwitchCollector)
}

// NewWallSwitchCollector builds the wall switch collector and its metric
// descriptors.
func NewWallSwitchCollector(logger *slog.Logger) (Collector, error) {
	const subsystem = "wallswitch"

	return &wallSwitchCollector{
		on: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "on"),
			"Whether the wall switch is switched on (1) or off (0).",
			deviceLabels, nil,
		),
		energy: newEnergyMetrics(),
		logger: logger,
	}, nil
}

// Update emits the state and energy readings of every wall switch.
func (c *wallSwitchCollector) Update(ch chan<- prometheus.Metric) error {
	results, err := snapshot(c.logger)
	if err != nil {
		c.logger.Error("Failed to build the device registry", "err", err)
		return err
	}

	readings, anyDevices := readingsOfType(results, kasa.TypeWallSwitch)
	if !anyDevices {
		return ErrNoData
	}

	for _, reading := range readings {
		labels := labelsFor(reading)
		emitGauge(ch, c.on, boolValue(reading.On), labels...)
		c.energy.emit(ch, reading.Energy, labels...)
	}

	return nil
}
