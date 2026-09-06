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

//go:build !nodimmer

package collector

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// dimmerCollector reports dimmer wall switches — an HS220, a KS220. They are
// switches with a brightness and no colour, so only the two light metrics that
// mean anything for them are declared.
type dimmerCollector struct {
	light  lightMetrics
	energy energyMetrics
	logger *slog.Logger
}

func init() {
	registerCollector("dimmer", defaultEnabled, NewDimmerCollector)
}

// NewDimmerCollector builds the dimmer collector and its metric descriptors.
func NewDimmerCollector(logger *slog.Logger) (Collector, error) {
	return &dimmerCollector{
		light:  newLightMetrics("dimmer", "dimmer", false),
		energy: newEnergyMetrics(),
		logger: logger,
	}, nil
}

// Update emits the state, brightness and energy readings of every dimmer.
func (c *dimmerCollector) Update(ch chan<- prometheus.Metric) error {
	results, err := snapshot(c.logger)
	if err != nil {
		c.logger.Error("Failed to build the device registry", "err", err)
		return err
	}

	readings, anyDevices := readingsOfType(results, kasa.TypeDimmer)
	if !anyDevices {
		return ErrNoData
	}

	for _, reading := range readings {
		c.light.emit(ch, reading)
		c.energy.emit(ch, reading.Energy, labelsFor(reading)...)
	}

	return nil
}
