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
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// deviceCollector reports the facts every device has in common, whatever kind
// of hardware it is: whether the exporter can reach it, what it is, how good
// its signal is and how long it has been switched on.
type deviceCollector struct {
	up             *prometheus.Desc
	info           *prometheus.Desc
	signalStrength *prometheus.Desc
	signalLevel    *prometheus.Desc
	uptime         *prometheus.Desc
	ledOn          *prometheus.Desc
	overheated     *prometheus.Desc
	updating       *prometheus.Desc
	count          *prometheus.Desc
	logger         *slog.Logger
}

func init() {
	registerCollector("device", defaultEnabled, NewDeviceCollector)
}

// NewDeviceCollector builds the device collector and its metric descriptors.
func NewDeviceCollector(logger *slog.Logger) (Collector, error) {
	const subsystem = "device"

	return &deviceCollector{
		up: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "up"),
			"Whether the exporter could read the device on this scrape (1 = yes, 0 = no).",
			deviceLabels, nil,
		),
		info: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "info"),
			"Static device information. Always 1; read the labels.",
			append(append([]string{}, deviceLabels...),
				"model", "device_id", "hardware_version", "firmware_version", "mac", "type", "protocol"),
			nil,
		),
		signalStrength: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "signal_strength_dbm"),
			"Wi-Fi signal strength reported by the device, in dBm.",
			deviceLabels, nil,
		),
		signalLevel: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "signal_level"),
			"Wi-Fi signal quality as the device grades it, from 0 (worst) to 3 (best).",
			deviceLabels, nil,
		),
		uptime: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "uptime_seconds"),
			// Firmware counts this from the relay closing, not from boot, so
			// calling it boot time would invite an alert that fires every time
			// somebody switches the device off and on.
			"Seconds the device has been switched on.",
			deviceLabels, nil,
		),
		ledOn: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "led_on"),
			"Whether the device's status LED is lit (1) or off (0).",
			deviceLabels, nil,
		),
		overheated: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "overheated"),
			"Whether the device reports itself as overheated (1 = yes, 0 = no).",
			deviceLabels, nil,
		),
		updating: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "updating"),
			"Whether the device is applying a firmware update (1 = yes, 0 = no).",
			deviceLabels, nil,
		),
		count: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "devices"),
			"Number of devices the exporter is managing, by kind.",
			[]string{"type"}, nil,
		),
		logger: logger,
	}, nil
}

// Update emits one set of common metrics per device, reachable or not.
//
// A device that failed this scrape still reports kasa_device_up at 0 under its
// last known name. That is the point of the metric: a device that simply
// disappeared from the output is indistinguishable from one that was never
// configured, and cannot be alerted on.
func (c *deviceCollector) Update(ch chan<- prometheus.Metric) error {
	results, err := snapshot(c.logger)
	if err != nil {
		c.logger.Error("Failed to build the device registry", "err", err)
		return err
	}
	if len(results) == 0 {
		return ErrNoData
	}

	counts := make(map[kasa.DeviceType]int)

	for _, result := range results {
		if result.Reading == nil {
			ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0, result.Host, result.Alias)
			// Counted under the category it was last read as, so a device going
			// down does not move between buckets and make the fleet inventory
			// flap.
			counts[result.Type]++
			continue
		}

		reading := result.Reading
		labels := labelsFor(reading)
		counts[reading.Type]++

		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1, labels...)
		ch <- prometheus.MustNewConstMetric(
			c.info, prometheus.GaugeValue, 1,
			reading.Host, aliasOf(reading),
			reading.Model, reading.DeviceID, reading.HardwareVersion,
			reading.FirmwareVersion, reading.MAC, string(reading.Type), string(reading.Protocol),
		)

		emitGauge(ch, c.signalStrength, reading.RSSI, labels...)
		emitGauge(ch, c.signalLevel, reading.SignalLevel, labels...)
		emitGauge(ch, c.uptime, reading.UptimeSeconds, labels...)
		emitGauge(ch, c.ledOn, boolValue(reading.LEDOn), labels...)
		emitGauge(ch, c.overheated, boolValue(reading.Overheated), labels...)
		emitGauge(ch, c.updating, boolValue(reading.Updating), labels...)
	}

	for deviceType, count := range counts {
		ch <- prometheus.MustNewConstMetric(c.count, prometheus.GaugeValue, float64(count), string(deviceType))
	}

	return nil
}
