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

package kasa

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// The IOT dialect addresses a device by module: system, emeter and, on bulbs,
// the lighting service. Several modules can be asked for in one request, which
// is what keeps a plug to a single round trip per scrape.
const (
	iotEmeterModule     = "emeter"
	iotBulbEmeterModule = "smartlife.iot.common.emeter"
)

// transport is the raw request/response channel underneath a dialect: XOR over
// TCP, or KLAP over HTTP.
type transport interface {
	Query(ctx context.Context, request []byte) ([]byte, error)
	Close()
}

// iotQuerier speaks the IOT dialect over any transport.
type iotQuerier struct {
	transport transport

	// emeterModule is the module name that answered last time. Bulbs keep their
	// energy readings under a different name from plugs, and the device does
	// not say which it uses, so the working name is remembered after the first
	// read rather than rediscovered on every scrape.
	emeterModule string
	// deviceHasEmeter goes false once the device has shown it has no
	// device-level energy readings — an HS300 reports per-outlet only — so
	// later scrapes stop asking for a module that will never answer.
	deviceHasEmeter bool
}

// newIOTQuerier builds an IOT dialect over the given transport.
func newIOTQuerier(t transport) *iotQuerier {
	return &iotQuerier{transport: t, emeterModule: iotEmeterModule, deviceHasEmeter: true}
}

// iotChild is one outlet of a power strip as sysinfo reports it.
type iotChild struct {
	ID     string   `json:"id"`
	Alias  string   `json:"alias"`
	State  *int     `json:"state"`
	OnTime *float64 `json:"on_time"`
}

// iotLightState is a bulb's current light settings. DefaultOnState holds the
// settings the bulb will return to, which is the only place they live while the
// bulb is off.
type iotLightState struct {
	OnOff          *int           `json:"on_off"`
	Hue            *float64       `json:"hue"`
	Saturation     *float64       `json:"saturation"`
	ColorTemp      *float64       `json:"color_temp"`
	Brightness     *float64       `json:"brightness"`
	DefaultOnState *iotLightState `json:"dft_on_state"`
}

// iotSysinfo is the subset of an IOT device's sysinfo the exporter reports.
// Several fields have two spellings across the range — a bulb reports mic_mac
// and mic_type where a plug reports mac and type — so both are decoded and
// resolved when the reading is built.
type iotSysinfo struct {
	SWVer      string         `json:"sw_ver"`
	HWVer      string         `json:"hw_ver"`
	Model      string         `json:"model"`
	DeviceID   string         `json:"deviceId"`
	Alias      string         `json:"alias"`
	MAC        string         `json:"mac"`
	MicMAC     string         `json:"mic_mac"`
	Type       string         `json:"type"`
	MicType    string         `json:"mic_type"`
	Feature    string         `json:"feature"`
	RSSI       *float64       `json:"rssi"`
	RelayState *int           `json:"relay_state"`
	OnTime     *float64       `json:"on_time"`
	LEDOff     *int           `json:"led_off"`
	Updating   *int           `json:"updating"`
	Brightness *float64       `json:"brightness"`
	Length     *float64       `json:"length"`
	LightState *iotLightState `json:"light_state"`
	Children   []iotChild     `json:"children"`
	ErrCode    *int           `json:"err_code"`
}

// family returns the device family string, whichever spelling this model uses.
func (s *iotSysinfo) family() string {
	if s.MicType != "" {
		return s.MicType
	}
	return s.Type
}

// mac returns the hardware address, whichever spelling this model uses.
func (s *iotSysinfo) mac() string {
	if s.MAC != "" {
		return s.MAC
	}
	return s.MicMAC
}

// iotEmeter is an energy reading. Firmware picks its own units — an HS300
// reports milliwatts where an older HS110 reports watts, sometimes for the same
// field name across hardware revisions — so both spellings of each quantity are
// decoded and normalised in one place.
type iotEmeter struct {
	CurrentMA *float64 `json:"current_ma"`
	Current   *float64 `json:"current"`
	VoltageMV *float64 `json:"voltage_mv"`
	Voltage   *float64 `json:"voltage"`
	PowerMW   *float64 `json:"power_mw"`
	Power     *float64 `json:"power"`
	TotalWH   *float64 `json:"total_wh"`
	Total     *float64 `json:"total"`
	EnergyWH  *float64 `json:"energy_wh"`
	Energy    *float64 `json:"energy"`
	ErrCode   *int     `json:"err_code"`
}

// energy converts a raw reading to SI units, or returns nil when the module
// answered with an error or reported nothing usable.
func (e *iotEmeter) energy() *Energy {
	if e == nil || (e.ErrCode != nil && *e.ErrCode != 0) {
		return nil
	}

	out := &Energy{}
	// Each quantity takes the first spelling present, scaled to its SI unit.
	switch {
	case e.PowerMW != nil:
		out.PowerWatts = float(*e.PowerMW / 1000)
	case e.Power != nil:
		out.PowerWatts = float(*e.Power)
	}
	switch {
	case e.VoltageMV != nil:
		out.VoltageVolts = float(*e.VoltageMV / 1000)
	case e.Voltage != nil:
		out.VoltageVolts = float(*e.Voltage)
	}
	switch {
	case e.CurrentMA != nil:
		out.CurrentAmperes = float(*e.CurrentMA / 1000)
	case e.Current != nil:
		out.CurrentAmperes = float(*e.Current)
	}
	switch {
	case e.TotalWH != nil:
		out.TotalKWh = float(*e.TotalWH / 1000)
	case e.Total != nil:
		out.TotalKWh = float(*e.Total)
	case e.EnergyWH != nil:
		out.TotalKWh = float(*e.EnergyWH / 1000)
	case e.Energy != nil:
		out.TotalKWh = float(*e.Energy)
	}

	if out.PowerWatts == nil && out.VoltageVolts == nil && out.CurrentAmperes == nil && out.TotalKWh == nil {
		return nil
	}
	return out
}

// iotResponse is the shape of every IOT reply: modules at the top level, each
// holding the answers to the methods that were asked of it.
type iotResponse struct {
	System *struct {
		GetSysinfo *iotSysinfo `json:"get_sysinfo"`
	} `json:"system"`
	Emeter          *iotEmeterResponse `json:"emeter"`
	SmartlifeEmeter *iotEmeterResponse `json:"smartlife.iot.common.emeter"`
}

// iotEmeterResponse wraps an emeter module's answer, which may be a reading or a
// module-level error.
type iotEmeterResponse struct {
	GetRealtime *iotEmeter `json:"get_realtime"`
	ErrCode     *int       `json:"err_code"`
}

// realtime returns the reading this module answered with, if any.
func (m *iotEmeterResponse) realtime() *iotEmeter {
	if m == nil || (m.ErrCode != nil && *m.ErrCode != 0) {
		return nil
	}
	return m.GetRealtime
}

// query sends one IOT request and decodes the reply.
func (q *iotQuerier) query(ctx context.Context, request any) (*iotResponse, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	raw, err := q.transport.Query(ctx, body)
	if err != nil {
		return nil, err
	}
	var response iotResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("unexpected response from device: %w", err)
	}
	return &response, nil
}

// Read fetches sysinfo and, where the device has them, energy readings.
//
// The two are asked for in a single request when the device has a device-level
// emeter, so the common case — a plug — costs one round trip. A strip is the
// exception: its outlets each carry their own meter and have to be read one at
// a time, addressed through the request's child context.
func (q *iotQuerier) Read(ctx context.Context) (*Reading, error) {
	request := map[string]any{"system": map[string]any{"get_sysinfo": map[string]any{}}}
	if q.deviceHasEmeter {
		request[q.emeterModule] = map[string]any{"get_realtime": map[string]any{}}
	}

	response, err := q.query(ctx, request)
	if err != nil {
		return nil, err
	}
	if response.System == nil || response.System.GetSysinfo == nil {
		return nil, fmt.Errorf("device did not return system information")
	}
	sysinfo := response.System.GetSysinfo
	if sysinfo.ErrCode != nil && *sysinfo.ErrCode != 0 {
		return nil, fmt.Errorf("device returned error code %d for sysinfo", *sysinfo.ErrCode)
	}

	reading := iotReading(sysinfo)

	if reading.Type == TypeStrip {
		// An HS300 has no meter of its own; its total is the sum of its
		// outlets, so asking the device level for one only wastes a request.
		q.deviceHasEmeter = false
		if err := q.readSockets(ctx, sysinfo, reading); err != nil {
			return nil, err
		}
		return reading, nil
	}

	energy := response.Emeter.realtime()
	if energy == nil {
		energy = response.SmartlifeEmeter.realtime()
	}
	if energy == nil && q.deviceHasEmeter {
		// The module that was asked did not answer. A bulb keeps its readings
		// under a different module name, so try that one before concluding the
		// device has no meter at all.
		if follow := q.retryEmeter(ctx, reading.Type); follow != nil {
			energy = follow
		}
	}
	reading.Energy = energy.energy()

	return reading, nil
}

// retryEmeter asks the other emeter module name once, remembering the answer
// for later scrapes: a hit switches this device over to that module, a miss
// stops the device being asked for energy at all.
func (q *iotQuerier) retryEmeter(ctx context.Context, deviceType DeviceType) *iotEmeter {
	// Only lighting hardware keeps its readings under the alternate name, so
	// for anything else a silent emeter module means the device has no meter
	// and a second round trip would be wasted on every scrape from here on.
	if deviceType != TypeBulb && deviceType != TypeLightStrip {
		q.deviceHasEmeter = false
		return nil
	}

	other := iotBulbEmeterModule
	if q.emeterModule == iotBulbEmeterModule {
		other = iotEmeterModule
	}

	response, err := q.query(ctx, map[string]any{other: map[string]any{"get_realtime": map[string]any{}}})
	if err != nil {
		return nil
	}
	energy := response.Emeter.realtime()
	if energy == nil {
		energy = response.SmartlifeEmeter.realtime()
	}
	if energy == nil {
		q.deviceHasEmeter = false
		return nil
	}
	q.emeterModule = other
	return energy
}

// readSockets fills in a strip's outlets and derives the strip's own totals
// from them.
//
// One unreadable outlet does not fail the strip: the remaining outlets are
// still worth reporting, and the socket that failed simply has no energy series
// for this scrape. Losing every outlet is a different matter and is returned as
// an error, because a strip reporting nothing is a strip the operator needs to
// know about.
func (q *iotQuerier) readSockets(ctx context.Context, sysinfo *iotSysinfo, reading *Reading) error {
	if len(sysinfo.Children) == 0 {
		return nil
	}

	total := &Energy{}
	measured := false
	failures := 0

	for i, child := range sysinfo.Children {
		socket := Socket{
			// Position on the strip, counted from 1. Unused outlets share the
			// alias "Empty", so naming them would collapse them into one series.
			ID:    strconv.Itoa(i + 1),
			Alias: child.Alias,
		}
		if child.State != nil {
			socket.On = boolean(*child.State != 0)
		}
		if child.OnTime != nil {
			socket.UptimeSeconds = float(*child.OnTime)
		}

		energy, err := q.readSocketEnergy(ctx, sysinfo, child)
		if err != nil {
			failures++
		} else if energy != nil {
			socket.Energy = energy
			measured = true
			addEnergy(total, energy)
		}

		reading.Sockets = append(reading.Sockets, socket)
	}

	if failures == len(sysinfo.Children) {
		return fmt.Errorf("no outlet of the strip could be read")
	}
	if measured {
		reading.Energy = total
	}
	return nil
}

// readSocketEnergy fetches one outlet's meter, addressing it through the
// request's child context.
func (q *iotQuerier) readSocketEnergy(ctx context.Context, sysinfo *iotSysinfo, child iotChild) (*Energy, error) {
	childID := child.ID
	// Some firmware reports the outlet's position alone rather than its full
	// identifier; the device only answers to the full one.
	if len(childID) <= 2 {
		childID = sysinfo.DeviceID + childID
	}

	response, err := q.query(ctx, map[string]any{
		"context": map[string]any{"child_ids": []string{childID}},
		"emeter":  map[string]any{"get_realtime": map[string]any{}},
	})
	if err != nil {
		return nil, err
	}
	return response.Emeter.realtime().energy(), nil
}

// Close releases the underlying transport.
func (q *iotQuerier) Close() { q.transport.Close() }

// iotReading turns sysinfo into the protocol-independent reading.
func iotReading(s *iotSysinfo) *Reading {
	reading := &Reading{
		Alias:            s.Alias,
		Model:            s.Model,
		DeviceID:         s.DeviceID,
		HardwareVersion:  s.HWVer,
		FirmwareVersion:  s.SWVer,
		MAC:              s.mac(),
		Type:             deviceTypeFor(s.Model, s.family()),
		Protocol:         ProtocolIOT,
		RSSI:             s.RSSI,
		UptimeSeconds:    s.OnTime,
		Brightness:       s.Brightness,
		LightStripLength: s.Length,
	}

	if s.RelayState != nil {
		reading.On = boolean(*s.RelayState != 0)
	}
	if s.LEDOff != nil {
		// Reported as "led_off", so it is inverted to read as "the LED is lit".
		reading.LEDOn = boolean(*s.LEDOff == 0)
	}
	if s.Updating != nil {
		reading.Updating = boolean(*s.Updating != 0)
	}

	if s.LightState != nil {
		applyLightState(reading, s.LightState)
	}

	return reading
}

// applyLightState copies a bulb's light settings onto the reading.
//
// While a bulb is off its current settings are all zero and the real ones sit
// under dft_on_state — the values it will come back to. Those are what get
// reported, so a dimmed bulb does not appear to have been reset to nothing
// every time it is switched off; the separate on/off metric is what says
// whether the light is actually lit.
func applyLightState(reading *Reading, state *iotLightState) {
	if state.OnOff != nil {
		reading.On = boolean(*state.OnOff != 0)
	}

	active := state
	if state.OnOff != nil && *state.OnOff == 0 && state.DefaultOnState != nil {
		active = state.DefaultOnState
	}

	reading.Hue = active.Hue
	reading.Saturation = active.Saturation
	reading.ColorTempKelvin = active.ColorTemp
	if active.Brightness != nil {
		reading.Brightness = active.Brightness
	}
}

// addEnergy accumulates one reading into a running total, used to derive a
// strip's figures from its outlets. A quantity absent from every outlet stays
// absent from the total rather than becoming a zero.
func addEnergy(total, add *Energy) {
	if add == nil {
		return
	}
	addFloat(&total.PowerWatts, add.PowerWatts)
	addFloat(&total.CurrentAmperes, add.CurrentAmperes)
	addFloat(&total.TotalKWh, add.TotalKWh)
	// Voltage is shared by every outlet on a strip, so it is taken rather than
	// summed: adding six readings of 120 V would report 720 V.
	if total.VoltageVolts == nil && add.VoltageVolts != nil {
		total.VoltageVolts = float(*add.VoltageVolts)
	}
}

// addFloat adds v into the pointed-to total, creating it if this is the first
// contribution.
func addFloat(total **float64, v *float64) {
	if v == nil {
		return
	}
	if *total == nil {
		*total = float(*v)
		return
	}
	**total += *v
}
