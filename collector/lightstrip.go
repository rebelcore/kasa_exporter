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

//go:build !nolightstrip

package collector

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// lightStripCollector reports addressable light strips — the KL400 and KL430
// ranges. They behave as bulbs with a length, which is the one reading a bulb
// has no equivalent of.
type lightStripCollector struct {
	light  lightMetrics
	length *prometheus.Desc
	energy energyMetrics
	logger *slog.Logger
}

func init() {
	registerCollector("lightstrip", defaultEnabled, NewLightStripCollector)
}

// NewLightStripCollector builds the light strip collector and its metric
// descriptors.
func NewLightStripCollector(logger *slog.Logger) (Collector, error) {
	const subsystem = "lightstrip"

	return &lightStripCollector{
		light: newLightMetrics(subsystem, "light strip", true),
		length: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "length"),
			"Number of addressable segments the light strip reports.",
			deviceLabels, nil,
		),
		energy: newEnergyMetrics(),
		logger: logger,
	}, nil
}

// Update emits the light settings, length and energy readings of every light
// strip.
func (c *lightStripCollector) Update(ch chan<- prometheus.Metric) error {
	results, err := snapshot(c.logger)
	if err != nil {
		c.logger.Error("Failed to build the device registry", "err", err)
		return err
	}

	readings, anyDevices := readingsOfType(results, kasa.TypeLightStrip)
	if !anyDevices {
		return ErrNoData
	}

	for _, reading := range readings {
		labels := labelsFor(reading)
		c.light.emit(ch, reading)
		emitGauge(ch, c.length, reading.LightStripLength, labels...)
		c.energy.emit(ch, reading.Energy, labels...)
	}

	return nil
}
