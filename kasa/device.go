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

// Package kasa speaks the TP-Link Kasa and Tapo device protocols.
//
// Three transports are implemented, because a single fleet routinely mixes all
// of them: the legacy XOR-obfuscated protocol on TCP 9999 (an HS300 on original
// firmware), KLAP over HTTP (an EP25, and an HS300 on hardware 2.0), and the
// older secure-passthrough AES exchange (Tapo-generation firmware). Two message
// dialects ride on top: the IOT dialect of `{"system":{"get_sysinfo":{}}}` and
// the SMART dialect of `{"method":"get_device_info"}`.
//
// Discover finds devices on the LAN, Connect reaches one by address, and both
// return a *Device whose Read method produces the protocol-independent Reading
// that the collectors turn into metrics. Nothing above this package needs to
// know which transport a device speaks.
package kasa

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/version"
)

// maxResponseBytes caps how much of a device response is read into memory. It
// is far larger than any legitimate reply — the biggest is an HS300's sysinfo
// with six children — so valid data is never truncated, while a misbehaving
// device cannot make the exporter allocate without bound. It is a var only so
// tests can shrink it.
var maxResponseBytes int64 = 8 << 20 // 8 MiB

// terminalUUID identifies this exporter in the SMART dialect's request
// envelope. The firmware only requires that it be present and stable for a
// session; it is not a credential and carries nothing about the host.
const terminalUUID = "00000000-0000-0000-0000-000000000000"

// userAgent identifies the exporter to devices that log their callers,
// including the build version when one has been stamped in via ldflags.
func userAgent() string {
	if v := version.Version; v != "" {
		return "kasa_exporter/" + v
	}
	return "kasa_exporter"
}

// DeviceType is the category a device falls into. It decides which collector
// reports the device, so every device resolves to exactly one value.
type DeviceType string

// The device categories the exporter reports. TypeUnknown covers hardware whose
// model is not recognised: it is still reported by the device collector, so a
// new model shows up as a reachable device rather than vanishing.
const (
	TypePlug       DeviceType = "plug"
	TypeStrip      DeviceType = "strip"
	TypeBulb       DeviceType = "bulb"
	TypeLightStrip DeviceType = "lightstrip"
	TypeDimmer     DeviceType = "dimmer"
	TypeWallSwitch DeviceType = "wallswitch"
	TypeHub        DeviceType = "hub"
	TypeUnknown    DeviceType = "unknown"
)

// Protocol names the dialect a device speaks, reported as a label on
// kasa_device_info so an operator can tell at a glance which half of the fleet
// a device belongs to.
type Protocol string

// The two message dialects. They are independent of the transport: an HS300 is
// ProtocolIOT whether it is reached over XOR or KLAP.
const (
	ProtocolIOT   Protocol = "iot"
	ProtocolSMART Protocol = "smart"
)

// Energy is one emeter reading, normalised to SI units. Every field is a
// pointer because firmware reports whichever subset it supports — an EP25 has
// no voltage or current, an HS300 socket has all four — and a missing reading
// must be left unexported rather than reported as zero.
type Energy struct {
	PowerWatts     *float64
	VoltageVolts   *float64
	CurrentAmperes *float64
	TotalKWh       *float64
}

// Socket is one outlet of a power strip. ID is the outlet's position on the
// strip, counted from 1, rather than a lookup by name: unused outlets on an
// HS300 all share the alias "Empty" and would otherwise collide into a single
// series.
type Socket struct {
	ID            string
	Alias         string
	On            *bool
	UptimeSeconds *float64
	Energy        *Energy
}

// Child is a device paired to a hub — a sensor, button or thermostat. The
// readings are all optional because they vary by child model.
type Child struct {
	ID                 string
	Alias              string
	Model              string
	Category           string
	Online             *bool
	BatteryPercent     *float64
	BatteryLow         *bool
	TemperatureCelsius *float64
	HumidityPercent    *float64
	SignalLevel        *float64
	RSSI               *float64
}

// Reading is one device's complete state at a point in time, in the shape the
// collectors consume. It deliberately flattens the differences between the IOT
// and SMART dialects so no collector has to know which one produced it.
type Reading struct {
	Host            string
	Alias           string
	Model           string
	DeviceID        string
	HardwareVersion string
	FirmwareVersion string
	MAC             string
	Type            DeviceType
	Protocol        Protocol

	On            *bool
	LEDOn         *bool
	Overheated    *bool
	Updating      *bool
	UptimeSeconds *float64
	RSSI          *float64
	SignalLevel   *float64

	// Brightness through LightStripLength are lighting properties, set only by
	// bulbs, light strips and dimmers.
	Brightness       *float64
	ColorTempKelvin  *float64
	Hue              *float64
	Saturation       *float64
	LightStripLength *float64

	Energy   *Energy
	Sockets  []Socket
	Children []Child
}

// querier is one negotiated way of talking to a device: a transport paired with
// the dialect that rides on it.
type querier interface {
	// Read fetches the device's current state.
	Read(ctx context.Context) (*Reading, error)
	// Close releases any session the transport holds.
	Close()
}

// Device is a device the exporter manages. It owns the negotiated connection
// and is reused across scrapes, so an expensive KLAP handshake is paid once
// rather than on every collection.
type Device struct {
	host string

	mu      sync.Mutex
	querier querier
	// alias and deviceType are remembered from the last successful read so a
	// device that has since become unreachable is still reported under its name
	// and in its own category. Falling back to the bare address, or to
	// TypeUnknown, would move the device to a new series at the moment it
	// fails — losing it from any dashboard grouped by either, exactly when it
	// matters.
	alias      string
	deviceType DeviceType
	// discovered holds what discovery already told us about the device, so a
	// connection can be rebuilt without another broadcast.
	discovered *discoveryResult
	creds      Credentials
	timeout    time.Duration
}

// Host returns the address the device is reached at.
func (d *Device) Host() string { return d.host }

// Alias returns the device's name, falling back to its address until a
// successful read has supplied one.
func (d *Device) Alias() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.alias != "" {
		return d.alias
	}
	return d.host
}

// Type returns the device's category, TypeUnknown until a successful read has
// established one.
func (d *Device) Type() DeviceType {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deviceType == "" {
		return TypeUnknown
	}
	return d.deviceType
}

// Read fetches the device's current state, establishing the connection on first
// use. A failure drops the connection so the next scrape negotiates afresh,
// which is what lets a device recover from a reboot without restarting the
// exporter.
func (d *Device) Read(ctx context.Context) (*Reading, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.querier == nil {
		q, err := negotiate(ctx, d.host, d.creds, d.timeout, d.discovered)
		if err != nil {
			return nil, err
		}
		d.querier = q
	}

	reading, err := d.querier.Read(ctx)
	if err != nil {
		d.querier.Close()
		d.querier = nil
		return nil, err
	}

	if reading.Alias != "" {
		d.alias = reading.Alias
	}
	if reading.Type != "" {
		d.deviceType = reading.Type
	}
	reading.Host = d.host
	return reading, nil
}

// Close releases the device's connection.
func (d *Device) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.querier != nil {
		d.querier.Close()
		d.querier = nil
	}
}

// deviceTypeFor maps a model name and the device family the firmware reports
// onto the category that decides which collector owns the device.
//
// The model is checked first because it is the more specific signal: an HS300
// and an HS110 both report the IOT smart-plug family, but only one of them has
// outlets. The family is the fallback that keeps an unrecognised model in a
// sensible category rather than in TypeUnknown.
func deviceTypeFor(model, family string) DeviceType {
	m := strings.ToUpper(model)
	// Region and hardware suffixes ("EP25(US)", "HS300(US) 2.0") carry no type
	// information and would otherwise defeat the prefix matches below.
	if i := strings.IndexAny(m, "( "); i > 0 {
		m = m[:i]
	}

	switch {
	// Power strips and multi-outlet adapters, the models with sockets.
	case strings.HasPrefix(m, "HS300"), strings.HasPrefix(m, "HS107"),
		strings.HasPrefix(m, "KP200"), strings.HasPrefix(m, "KP303"),
		strings.HasPrefix(m, "KP400"), strings.HasPrefix(m, "EP40"),
		strings.HasPrefix(m, "P300"), strings.HasPrefix(m, "P304"),
		strings.HasPrefix(m, "P306"), strings.HasPrefix(m, "TP25"):
		return TypeStrip

	// Hubs, which report sensors rather than readings of their own.
	case strings.HasPrefix(m, "KH100"), strings.HasPrefix(m, "H100"),
		strings.HasPrefix(m, "H200"), strings.HasPrefix(m, "H300"):
		return TypeHub

	// Light strips, distinguished from bulbs by having a length.
	case strings.HasPrefix(m, "KL400"), strings.HasPrefix(m, "KL420"),
		strings.HasPrefix(m, "KL430"), strings.HasPrefix(m, "L900"),
		strings.HasPrefix(m, "L920"), strings.HasPrefix(m, "L930"):
		return TypeLightStrip

	// Dimmers: wall switches that also report a brightness.
	case strings.HasPrefix(m, "HS220"), strings.HasPrefix(m, "KS220"),
		strings.HasPrefix(m, "KS230"), strings.HasPrefix(m, "ES20"),
		strings.HasPrefix(m, "S500D"), strings.HasPrefix(m, "S505D"):
		return TypeDimmer

	// Wall switches with no brightness control.
	case strings.HasPrefix(m, "HS200"), strings.HasPrefix(m, "HS210"),
		strings.HasPrefix(m, "KS200"), strings.HasPrefix(m, "KS205"),
		strings.HasPrefix(m, "KS225"), strings.HasPrefix(m, "KS240"),
		strings.HasPrefix(m, "S500"), strings.HasPrefix(m, "S505"),
		strings.HasPrefix(m, "S210"), strings.HasPrefix(m, "S220"):
		return TypeWallSwitch

	// Bulbs.
	case strings.HasPrefix(m, "KL"), strings.HasPrefix(m, "LB"),
		strings.HasPrefix(m, "L510"), strings.HasPrefix(m, "L520"),
		strings.HasPrefix(m, "L530"), strings.HasPrefix(m, "L535"),
		strings.HasPrefix(m, "L610"), strings.HasPrefix(m, "L630"):
		return TypeBulb

	// Plugs, the largest group: everything from an HS100 to an EP25.
	case strings.HasPrefix(m, "HS1"), strings.HasPrefix(m, "KP1"),
		strings.HasPrefix(m, "EP1"), strings.HasPrefix(m, "EP2"),
		strings.HasPrefix(m, "P100"), strings.HasPrefix(m, "P110"),
		strings.HasPrefix(m, "P115"), strings.HasPrefix(m, "P125"),
		strings.HasPrefix(m, "P135"), strings.HasPrefix(m, "TP15"):
		return TypePlug
	}

	// No model match, so fall back to the family the firmware advertises.
	f := strings.ToUpper(family)
	switch {
	case strings.Contains(f, "HUB"):
		return TypeHub
	case strings.Contains(f, "SWITCH"):
		return TypeWallSwitch
	case strings.Contains(f, "BULB"), strings.Contains(f, "LIGHT"):
		return TypeBulb
	case strings.Contains(f, "PLUG"):
		return TypePlug
	}
	return TypeUnknown
}

// float returns a pointer to v. Reading's numeric fields are pointers so an
// absent value stays absent, and building those inline is unreadable.
func float(v float64) *float64 { return &v }

// boolean returns a pointer to v, for the same reason as float.
func boolean(v bool) *bool { return &v }
