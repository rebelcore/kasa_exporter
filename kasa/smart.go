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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// The SMART dialect addresses a device by method name rather than by module.
// Several methods can be batched into one multipleRequest, which is what keeps
// an EP25 to a single round trip even though its state and its energy readings
// live behind different methods.
const (
	smartGetDeviceInfo      = "get_device_info"
	smartGetEnergyUsage     = "get_energy_usage"
	smartGetCurrentPower    = "get_current_power"
	smartGetChildDeviceList = "get_child_device_list"
	smartMultipleRequest    = "multipleRequest"
)

// smartRequestID numbers requests. The firmware only requires that the value
// change between requests; it is shared across devices because nothing
// correlates a response by anything but its position in the batch.
var smartRequestID atomic.Int64

// smartQuerier speaks the SMART dialect over any transport.
type smartQuerier struct {
	transport transport

	// Which optional methods this device answers. They start true and are
	// switched off the first time the device reports the method as unsupported,
	// so a plug without a meter stops being asked for one on every scrape.
	wantsEnergy   bool
	wantsPower    bool
	wantsChildren bool
}

// newSMARTQuerier builds a SMART dialect over the given transport.
func newSMARTQuerier(t transport) *smartQuerier {
	return &smartQuerier{transport: t, wantsEnergy: true, wantsPower: true, wantsChildren: true}
}

// smartEnvelope is the outer shape of every SMART reply.
type smartEnvelope struct {
	ErrorCode int             `json:"error_code"`
	Result    json.RawMessage `json:"result"`
}

// smartBatchResponse is one member of a multipleRequest reply. Each member
// carries its own error code, so an unsupported method fails alone rather than
// failing the batch.
type smartBatchResponse struct {
	Method    string          `json:"method"`
	ErrorCode int             `json:"error_code"`
	Result    json.RawMessage `json:"result"`
}

// smartDeviceInfo is the subset of get_device_info the exporter reports.
//
// Nickname and SSID arrive base64-encoded, which is why they are decoded rather
// than used directly: a device called "Rack A" reports "UmFjayBB".
type smartDeviceInfo struct {
	DeviceID    string   `json:"device_id"`
	Model       string   `json:"model"`
	Type        string   `json:"type"`
	HWVer       string   `json:"hw_ver"`
	FWVer       string   `json:"fw_ver"`
	MAC         string   `json:"mac"`
	Nickname    string   `json:"nickname"`
	DeviceOn    *bool    `json:"device_on"`
	OnTime      *float64 `json:"on_time"`
	Overheated  *bool    `json:"overheated"`
	RSSI        *float64 `json:"rssi"`
	SignalLevel *float64 `json:"signal_level"`
	Brightness  *float64 `json:"brightness"`
	ColorTemp   *float64 `json:"color_temp"`
	Hue         *float64 `json:"hue"`
	Saturation  *float64 `json:"saturation"`
	LEDOff      *int     `json:"led_off"`
	// Position is set on a strip's child sockets and gives the outlet's place
	// on the strip.
	Position *int `json:"position"`
	// Category names a hub child's kind, for example "subg.trigger.temp-hmdt-sensor".
	Category          string   `json:"category"`
	Status            string   `json:"status"`
	AtLowBattery      *bool    `json:"at_low_battery"`
	BatteryPercentage *float64 `json:"battery_percentage"`
	CurrentTemp       *float64 `json:"current_temp"`
	CurrentHumidity   *float64 `json:"current_humidity"`
	TempUnit          string   `json:"temp_unit"`
}

// alias returns the device's name, decoding the base64 the firmware wraps it
// in.
//
// A name that does not decode is taken verbatim, since some firmware sends it
// in the clear. So is one that decodes to something unprintable: a short
// plaintext name such as "Hall" is also valid base64, and decoding it would
// turn a perfectly good label into three bytes of binary. Requiring the result
// to be readable text separates the two cases without having to know which
// firmware sent it.
func (i *smartDeviceInfo) alias() string {
	if i.Nickname == "" {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(i.Nickname)
	if err != nil || !isPrintable(decoded) {
		return i.Nickname
	}
	return string(decoded)
}

// isPrintable reports whether b is non-empty, valid UTF-8, and free of control
// characters.
func isPrintable(b []byte) bool {
	if len(b) == 0 || !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// smartEnergyUsage is the get_energy_usage reply. current_power is milliwatts
// here, unlike get_current_power, which reports watts — a difference the
// firmware does not signal anywhere, so the two are decoded separately.
type smartEnergyUsage struct {
	CurrentPowerMW *float64 `json:"current_power"`
	TodayEnergyWH  *float64 `json:"today_energy"`
	MonthEnergyWH  *float64 `json:"month_energy"`
}

// smartCurrentPower is the get_current_power reply, in watts.
type smartCurrentPower struct {
	CurrentPowerW *float64 `json:"current_power"`
}

// smartChildList is the get_child_device_list reply: the sockets of a strip or
// the sensors paired to a hub.
type smartChildList struct {
	ChildDeviceList []smartDeviceInfo `json:"child_device_list"`
}

// smartRequest builds the request envelope for one method.
func smartRequest(method string, params any) map[string]any {
	request := map[string]any{
		"method":             method,
		"requestID":          smartRequestID.Add(1),
		"request_time_milis": time.Now().UnixMilli(),
		"terminal_uuid":      terminalUUID,
	}
	if params != nil {
		request["params"] = params
	}
	return request
}

// query sends one request and returns the result member of the reply.
func (q *smartQuerier) query(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	raw, err := q.transport.Query(ctx, body)
	if err != nil {
		return nil, err
	}

	var envelope smartEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("unexpected response from device: %w", err)
	}
	if envelope.ErrorCode != 0 {
		return nil, fmt.Errorf("device returned error code %d", envelope.ErrorCode)
	}
	return envelope.Result, nil
}

// batch sends several methods in one request and returns each method's result,
// keyed by method name. A method the device does not support is left out of the
// map rather than failing the batch.
func (q *smartQuerier) batch(ctx context.Context, methods []string) (map[string]json.RawMessage, error) {
	requests := make([]map[string]any, 0, len(methods))
	for _, method := range methods {
		requests = append(requests, map[string]any{"method": method})
	}

	result, err := q.query(ctx, smartRequest(smartMultipleRequest, map[string]any{"requests": requests}))
	if err != nil {
		return nil, err
	}

	var responses struct {
		Responses []smartBatchResponse `json:"responses"`
	}
	if err := json.Unmarshal(result, &responses); err != nil {
		return nil, fmt.Errorf("unexpected batch response from device: %w", err)
	}

	out := make(map[string]json.RawMessage, len(responses.Responses))
	for _, response := range responses.Responses {
		if response.ErrorCode == 0 && len(response.Result) > 0 {
			out[response.Method] = response.Result
		}
	}
	return out, nil
}

// Read fetches the device's state and, where it has them, its energy readings
// and children.
//
// Everything the device is currently believed to support goes into one batched
// request. get_device_info is mandatory: without it there is no reading at all,
// so a batch that comes back without it is retried as a plain single request
// before the scrape is given up on — a handful of older firmware answers
// multipleRequest with an empty batch rather than an error.
func (q *smartQuerier) Read(ctx context.Context) (*Reading, error) {
	methods := []string{smartGetDeviceInfo}
	if q.wantsEnergy {
		methods = append(methods, smartGetEnergyUsage)
	}
	if q.wantsPower {
		methods = append(methods, smartGetCurrentPower)
	}
	if q.wantsChildren {
		methods = append(methods, smartGetChildDeviceList)
	}

	results, err := q.batch(ctx, methods)
	if err != nil {
		return nil, err
	}

	rawInfo, ok := results[smartGetDeviceInfo]
	if !ok {
		rawInfo, err = q.query(ctx, smartRequest(smartGetDeviceInfo, nil))
		if err != nil {
			return nil, err
		}
	}

	var info smartDeviceInfo
	if err := json.Unmarshal(rawInfo, &info); err != nil {
		return nil, fmt.Errorf("unexpected device information: %w", err)
	}
	reading := smartReading(&info)

	// A method that answered once will answer again; one that stayed silent is
	// not asked for on the next scrape, which is what keeps a plug with no
	// meter from paying for two dead methods on every scrape.
	energy := q.readEnergy(results)
	if energy != nil {
		reading.Energy = energy
	}

	if raw, ok := results[smartGetChildDeviceList]; ok {
		q.applyChildren(reading, raw)
	} else if q.wantsChildren {
		q.wantsChildren = false
	}

	return reading, nil
}

// readEnergy merges the two energy methods into one reading, preferring
// get_current_power for the instantaneous figure: it reports watts directly,
// where get_energy_usage reports milliwatts and is the older of the two.
func (q *smartQuerier) readEnergy(results map[string]json.RawMessage) *Energy {
	out := &Energy{}

	if raw, ok := results[smartGetEnergyUsage]; ok {
		var usage smartEnergyUsage
		if err := json.Unmarshal(raw, &usage); err == nil {
			if usage.CurrentPowerMW != nil {
				out.PowerWatts = float(*usage.CurrentPowerMW / 1000)
			}
			// The device keeps a running total per month rather than since
			// installation, which is the closest thing it offers to a
			// cumulative meter.
			if usage.MonthEnergyWH != nil {
				out.TotalKWh = float(*usage.MonthEnergyWH / 1000)
			}
		}
	} else if q.wantsEnergy {
		q.wantsEnergy = false
	}

	if raw, ok := results[smartGetCurrentPower]; ok {
		var power smartCurrentPower
		if err := json.Unmarshal(raw, &power); err == nil && power.CurrentPowerW != nil {
			out.PowerWatts = float(*power.CurrentPowerW)
		}
	} else if q.wantsPower {
		q.wantsPower = false
	}

	if out.PowerWatts == nil && out.TotalKWh == nil {
		return nil
	}
	return out
}

// applyChildren attaches a device's children, as sockets for a strip and as
// paired sensors for a hub.
func (q *smartQuerier) applyChildren(reading *Reading, raw json.RawMessage) {
	var list smartChildList
	if err := json.Unmarshal(raw, &list); err != nil {
		return
	}
	if len(list.ChildDeviceList) == 0 {
		return
	}

	// A device that reports children but was not recognised from its model is a
	// strip: a hub is only ever identified by model, and nothing else in the
	// range has children.
	if reading.Type == TypeHub {
		reading.Children = smartChildren(list.ChildDeviceList)
		return
	}
	if reading.Type != TypeStrip {
		reading.Type = TypeStrip
	}
	reading.Sockets = smartSockets(list.ChildDeviceList)
}

// smartSockets turns a strip's children into outlets, numbered by their
// position on the strip so that unnamed outlets stay distinct.
func smartSockets(children []smartDeviceInfo) []Socket {
	sockets := make([]Socket, 0, len(children))
	for i, child := range children {
		id := strconv.Itoa(i + 1)
		if child.Position != nil {
			// The firmware counts positions from zero.
			id = strconv.Itoa(*child.Position + 1)
		}
		socket := Socket{ID: id, Alias: child.alias(), On: child.DeviceOn}
		if child.OnTime != nil {
			socket.UptimeSeconds = float(*child.OnTime)
		}
		sockets = append(sockets, socket)
	}
	return sockets
}

// smartChildren turns a hub's children into the sensors the hub collector
// reports.
func smartChildren(children []smartDeviceInfo) []Child {
	out := make([]Child, 0, len(children))
	for _, child := range children {
		entry := Child{
			ID:              child.DeviceID,
			Alias:           child.alias(),
			Model:           child.Model,
			Category:        child.Category,
			BatteryPercent:  child.BatteryPercentage,
			HumidityPercent: child.CurrentHumidity,
			SignalLevel:     child.SignalLevel,
			RSSI:            child.RSSI,
		}
		if child.Status != "" {
			entry.Online = boolean(strings.EqualFold(child.Status, "online"))
		}
		if child.AtLowBattery != nil {
			entry.BatteryLow = child.AtLowBattery
		}
		if child.CurrentTemp != nil {
			// Sensors report in whichever unit they are configured for, so a
			// Fahrenheit reading is converted rather than exported as if it
			// were Celsius.
			temp := *child.CurrentTemp
			if strings.EqualFold(child.TempUnit, "fahrenheit") {
				temp = (temp - 32) * 5 / 9
			}
			entry.TemperatureCelsius = float(temp)
		}
		out = append(out, entry)
	}
	return out
}

// Close releases the underlying transport.
func (q *smartQuerier) Close() { q.transport.Close() }

// smartReading turns device information into the protocol-independent reading.
func smartReading(i *smartDeviceInfo) *Reading {
	reading := &Reading{
		Alias:           i.alias(),
		Model:           i.Model,
		DeviceID:        i.DeviceID,
		HardwareVersion: i.HWVer,
		FirmwareVersion: i.FWVer,
		MAC:             i.MAC,
		Type:            deviceTypeFor(i.Model, i.Type),
		Protocol:        ProtocolSMART,
		On:              i.DeviceOn,
		Overheated:      i.Overheated,
		RSSI:            i.RSSI,
		SignalLevel:     i.SignalLevel,
		UptimeSeconds:   i.OnTime,
		Brightness:      i.Brightness,
		ColorTempKelvin: i.ColorTemp,
		Hue:             i.Hue,
		Saturation:      i.Saturation,
	}
	if i.LEDOff != nil {
		reading.LEDOn = boolean(*i.LEDOff == 0)
	}
	return reading
}
