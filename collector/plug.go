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

//go:build !noplug

package collector

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// plugCollector reports single-outlet smart plugs — an EP25, a KP125, an
// HS110 — and the energy the metered ones draw.
type plugCollector struct {
	on     *prometheus.Desc
	energy energyMetrics
	logger *slog.Logger
}

func init() {
	registerCollector("plug", defaultEnabled, NewPlugCollector)
}

// NewPlugCollector builds the plug collector and its metric descriptors.
func NewPlugCollector(logger *slog.Logger) (Collector, error) {
	const subsystem = "plug"

	return &plugCollector{
		on: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "on"),
			"Whether the plug is switched on (1) or off (0).",
			deviceLabels, nil,
		),
		energy: newEnergyMetrics(),
		logger: logger,
	}, nil
}

// Update emits the switch state and energy readings of every plug. A fleet with
// no plugs is a successful scrape with nothing to report; a scrape that found no
// devices at all is ErrNoData.
func (c *plugCollector) Update(ch chan<- prometheus.Metric) error {
	results, err := snapshot(c.logger)
	if err != nil {
		c.logger.Error("Failed to build the device registry", "err", err)
		return err
	}

	readings, anyDevices := readingsOfType(results, kasa.TypePlug)
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
