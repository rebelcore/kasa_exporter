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

//go:build !nobulb

package collector

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// bulbCollector reports smart bulbs — the KL and LB ranges — their light
// settings and, on the models that meter themselves, their power draw.
type bulbCollector struct {
	light  lightMetrics
	energy energyMetrics
	logger *slog.Logger
}

func init() {
	registerCollector("bulb", defaultEnabled, NewBulbCollector)
}

// NewBulbCollector builds the bulb collector and its metric descriptors.
func NewBulbCollector(logger *slog.Logger) (Collector, error) {
	return &bulbCollector{
		light:  newLightMetrics("bulb", "bulb", true),
		energy: newEnergyMetrics(),
		logger: logger,
	}, nil
}

// Update emits the light settings and energy readings of every bulb.
func (c *bulbCollector) Update(ch chan<- prometheus.Metric) error {
	results, err := snapshot(c.logger)
	if err != nil {
		c.logger.Error("Failed to build the device registry", "err", err)
		return err
	}

	readings, anyDevices := readingsOfType(results, kasa.TypeBulb)
	if !anyDevices {
		return ErrNoData
	}

	for _, reading := range readings {
		c.light.emit(ch, reading)
		c.energy.emit(ch, reading.Energy, labelsFor(reading)...)
	}

	return nil
}
