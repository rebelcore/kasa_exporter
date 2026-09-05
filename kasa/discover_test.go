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
	"bytes"
	"context"
	"encoding/hex"
	"testing"
	"time"
)

func TestDiscoveryProbeSMART(t *testing.T) {
	// The probe is a fixed packet, not a message with fields. A device that
	// receives anything else stays silent, so the bytes are pinned.
	if want, got := "020000010000000000000000463cb5d3", hex.EncodeToString(discoveryProbeSMART); got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestParseIOTDiscovery(t *testing.T) {
	// A reply on port 9999 is a full sysinfo under the obfuscation, and the
	// port itself is what says the device speaks the legacy transport.
	reply := xorEncrypt([]byte(hs300Sysinfo))

	got := parseDiscoveryReply("192.168.1.20", discoveryPortIOT, reply)
	if got == nil {
		t.Fatal("want a discovery result, got none")
	}
	if want, have := "HS300(US)", got.Model; want != have {
		t.Errorf("want model %q, have %q", want, have)
	}
	if want, have := "Rack A", got.Alias; want != have {
		t.Errorf("want alias %q, have %q", want, have)
	}
	if want, have := encryptXOR, got.EncryptType; want != have {
		t.Errorf("want encryption %q, have %q", want, have)
	}
	if want, have := ProtocolIOT, got.dialect(); want != have {
		t.Errorf("want dialect %q, have %q", want, have)
	}
	if want, have := klapLoginV1, got.loginVersion(); want != have {
		t.Errorf("want login version %d, have %d", want, have)
	}
}

func TestParseSMARTDiscovery(t *testing.T) {
	// The reply is a fixed 16-byte header followed by the JSON that names the
	// transport, the port and the login version.
	body := []byte(`{"error_code":0,"result":{
		"device_id":"802E1234","device_type":"SMART.KASAPLUG","device_model":"EP25(US)",
		"ip":"192.168.1.50","mac":"AA-BB-CC-DD-EE-FF",
		"mgt_encrypt_schm":{"is_support_https":false,"encrypt_type":"KLAP","http_port":80,"lv":2}
	}}`)
	reply := append(bytes.Repeat([]byte{0}, discoveryHeaderSMART), body...)

	got := parseDiscoveryReply("192.168.1.50", discoveryPortSMART, reply)
	if got == nil {
		t.Fatal("want a discovery result, got none")
	}
	if want, have := "EP25(US)", got.Model; want != have {
		t.Errorf("want model %q, have %q", want, have)
	}
	if want, have := encryptKLAP, got.EncryptType; want != have {
		t.Errorf("want encryption %q, have %q", want, have)
	}
	if want, have := ProtocolSMART, got.dialect(); want != have {
		t.Errorf("want dialect %q, have %q", want, have)
	}
	if want, have := klapLoginV2, got.loginVersion(); want != have {
		t.Errorf("want login version %d, have %d", want, have)
	}
	if want, have := 80, got.port(); want != have {
		t.Errorf("want port %d, have %d", want, have)
	}
}

func TestDiscoveryLoginVersionFromFirmware(t *testing.T) {
	// An IOT device on hardware 2.0 advertises login version 2. Defaulting it
	// to version 1 on family alone is what makes a correct password read as
	// wrong, so the advertised value must win.
	body := []byte(`{"error_code":0,"result":{
		"device_type":"IOT.SMARTPLUGSWITCH","device_model":"HS300(US)",
		"mgt_encrypt_schm":{"encrypt_type":"KLAP","http_port":80,"lv":2}
	}}`)
	reply := append(bytes.Repeat([]byte{0}, discoveryHeaderSMART), body...)

	got := parseDiscoveryReply("192.168.1.21", discoveryPortSMART, reply)
	if got == nil {
		t.Fatal("want a discovery result, got none")
	}
	if want, have := ProtocolIOT, got.dialect(); want != have {
		t.Errorf("want dialect %q, have %q", want, have)
	}
	if want, have := klapLoginV2, got.loginVersion(); want != have {
		t.Fatalf("want login version %d, have %d", want, have)
	}
}

func TestParseDiscoveryRejectsRubbish(t *testing.T) {
	tests := []struct {
		name string
		port int
		data []byte
	}{
		{name: "unknown port", port: 1234, data: []byte("{}")},
		{name: "iot, not json", port: discoveryPortIOT, data: xorEncrypt([]byte("hello"))},
		{name: "iot, no sysinfo", port: discoveryPortIOT, data: xorEncrypt([]byte(`{"emeter":{}}`))},
		{name: "smart, too short", port: discoveryPortSMART, data: bytes.Repeat([]byte{0}, discoveryHeaderSMART)},
		{
			name: "smart, not json",
			port: discoveryPortSMART,
			data: append(bytes.Repeat([]byte{0}, discoveryHeaderSMART), []byte("nope")...),
		},
		{
			name: "smart, error code",
			port: discoveryPortSMART,
			data: append(bytes.Repeat([]byte{0}, discoveryHeaderSMART), []byte(`{"error_code":-1}`)...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseDiscoveryReply("192.168.1.1", tt.port, tt.data); got != nil {
				t.Fatalf("want no result, got %+v", got)
			}
		})
	}
}

func TestDiscoveryDefaultsHTTPPort(t *testing.T) {
	// Firmware that omits the port is served on 80; one that supports HTTPS and
	// omits it is on 443.
	plain := &discoveryResult{EncryptType: encryptKLAP}
	if want, have := 80, plain.port(); want != have {
		t.Errorf("want port %d, have %d", want, have)
	}
	if want, have := "http://192.168.1.1:80/app", deviceURL("192.168.1.1", plain)+"/app"; want != have {
		t.Errorf("want %q, have %q", want, have)
	}

	secure := &discoveryResult{EncryptType: encryptKLAP, HTTPS: true}
	if want, have := "https://192.168.1.1:443/app", deviceURL("192.168.1.1", secure)+"/app"; want != have {
		t.Errorf("want %q, have %q", want, have)
	}
}

func TestDiscoverNoAnswer(t *testing.T) {
	// Discovery against the loopback address finds nothing, which must be an
	// empty result rather than an error: an empty network is not a failure.
	results, err := discover(t.Context(), "127.0.0.1", 100*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("want no results, got %d", len(results))
	}

	if _, err := discoverHost(t.Context(), "127.0.0.1", 100*time.Millisecond); err == nil {
		t.Fatal("want an error when a specific host does not answer")
	}
}

// withDiscoveryPorts points the probes at fake devices on loopback ports,
// restoring the real ones when the test ends.
func withDiscoveryPorts(t *testing.T, iot, smart int) {
	t.Helper()

	prevIOT, prevSMART := discoveryPortIOT, discoveryPortSMART
	discoveryPortIOT, discoveryPortSMART = iot, smart
	t.Cleanup(func() { discoveryPortIOT, discoveryPortSMART = prevIOT, prevSMART })
}

// smartDiscoveryReplyBytes builds a port-20002 answer: the fixed binary header
// followed by JSON.
func smartDiscoveryReplyBytes(body string) []byte {
	return append(bytes.Repeat([]byte{0}, discoveryHeaderSMART), []byte(body)...)
}

func TestDiscoverOverUDP(t *testing.T) {
	// The whole send/receive cycle against devices that really answer, rather
	// than bytes handed straight to the parser: this is what catches a probe
	// that is never sent or a reply that is read from the wrong port.
	iotDevice, iotPort := newUDPServer(t, func(request []byte) []byte {
		if want, have := `{"system":{"get_sysinfo":{}}}`, string(xorDecrypt(request)); want != have {
			t.Errorf("want probe %q, have %q", want, have)
		}
		return xorEncrypt([]byte(hs300Sysinfo))
	})
	smartDevice, smartPort := newUDPServer(t, func(request []byte) []byte {
		if !bytes.Equal(request, discoveryProbeSMART) {
			t.Errorf("want the fixed probe, have %x", request)
		}
		return smartDiscoveryReplyBytes(`{"error_code":0,"result":{
			"device_id":"802E1234","device_type":"SMART.KASAPLUG","device_model":"EP25(US)",
			"mgt_encrypt_schm":{"encrypt_type":"KLAP","http_port":80,"lv":2}
		}}`)
	})
	withDiscoveryPorts(t, iotPort, smartPort)

	results, err := discover(t.Context(), "127.0.0.1", 500*time.Millisecond, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both fakes are on 127.0.0.1, so they collapse into one entry keyed by
	// address — and the SMART reply must be the one kept, since it is the only
	// one that names the transport.
	if want, have := 1, len(results); want != have {
		t.Fatalf("want %d result, have %d: %+v", want, have, results)
	}
	if want, have := "EP25(US)", results[0].Model; want != have {
		t.Fatalf("want the SMART reply kept, have model %q", have)
	}
	if want, have := encryptKLAP, results[0].EncryptType; want != have {
		t.Errorf("want encryption %q, have %q", want, have)
	}

	// Two packets per sweep, because a broadcast datagram a device misses is
	// simply lost.
	if got := iotDevice.requestCount(); got != 2 {
		t.Errorf("want 2 probes on the legacy port, got %d", got)
	}
	if got := smartDevice.requestCount(); got != 2 {
		t.Errorf("want 2 probes on the SMART port, got %d", got)
	}
}

func TestDiscoverKeepsSMARTReplyWhateverTheOrder(t *testing.T) {
	// A device that answers on both ports must be recorded from the SMART
	// reply even when the legacy one arrives second, or it would be connected
	// over a transport its firmware has closed.
	_, smartPort := newUDPServer(t, func([]byte) []byte {
		return smartDiscoveryReplyBytes(`{"error_code":0,"result":{
			"device_type":"IOT.SMARTPLUGSWITCH","device_model":"HS300(US)",
			"mgt_encrypt_schm":{"encrypt_type":"KLAP","http_port":80,"lv":2}
		}}`)
	})
	_, iotPort := newUDPServer(t, func([]byte) []byte {
		// Delayed so the legacy answer is the later of the two.
		time.Sleep(50 * time.Millisecond)
		return xorEncrypt([]byte(hs300Sysinfo))
	})
	withDiscoveryPorts(t, iotPort, smartPort)

	results, err := discover(t.Context(), "127.0.0.1", 500*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := 1, len(results); want != have {
		t.Fatalf("want %d result, have %d", want, have)
	}
	if want, have := encryptKLAP, results[0].EncryptType; want != have {
		t.Fatalf("want the KLAP answer kept, have %q", have)
	}
	if want, have := klapLoginV2, results[0].loginVersion(); want != have {
		t.Errorf("want login version %d, have %d", want, have)
	}
}

func TestDiscoverIgnoresRubbishOnTheWire(t *testing.T) {
	// Something else on the port answering must not become a device.
	_, iotPort := newUDPServer(t, func([]byte) []byte { return []byte("not a device") })
	_, smartPort := newUDPServer(t, func([]byte) []byte { return []byte("nor is this") })
	withDiscoveryPorts(t, iotPort, smartPort)

	results, err := discover(t.Context(), "127.0.0.1", 300*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("want no results, got %+v", results)
	}
}

func TestDiscoverHostOverUDP(t *testing.T) {
	_, smartPort := newUDPServer(t, func([]byte) []byte {
		return smartDiscoveryReplyBytes(`{"error_code":0,"result":{
			"device_type":"SMART.KASAPLUG","device_model":"EP25(US)",
			"mgt_encrypt_schm":{"encrypt_type":"KLAP","http_port":80,"lv":2}
		}}`)
	})
	_, iotPort := newUDPServer(t, func([]byte) []byte { return nil })
	withDiscoveryPorts(t, iotPort, smartPort)

	found, err := discoverHost(t.Context(), "127.0.0.1", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := "EP25(US)", found.Model; want != have {
		t.Fatalf("want model %q, have %q", want, have)
	}
	// The reply's source address is the device's own, which for a host given as
	// a hostname would not match what was asked for, so the configured host is
	// what the device is reached at.
	if want, have := "127.0.0.1", found.Host; want != have {
		t.Errorf("want host %q, have %q", want, have)
	}
}

func TestDiscoverRespectsAContextDeadline(t *testing.T) {
	// A caller with less time than the discovery timeout must get control back
	// on its own deadline, not on the sweep's.
	_, iotPort := newUDPServer(t, func([]byte) []byte { return nil })
	_, smartPort := newUDPServer(t, func([]byte) []byte { return nil })
	withDiscoveryPorts(t, iotPort, smartPort)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := discover(ctx, "127.0.0.1", time.Minute, 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("discovery ignored the context deadline, took %s", elapsed)
	}
}

func TestDiscoverRejectsAnUnresolvableTarget(t *testing.T) {
	if _, err := discover(t.Context(), "no-such-host.invalid", 100*time.Millisecond, 1); err == nil {
		t.Fatal("want an error for a target that does not resolve")
	}
}

func TestDiscoverBuildsDevices(t *testing.T) {
	// The exported wrapper: each answer becomes a device carrying what
	// discovery already learned, so no second broadcast is needed to connect.
	_, smartPort := newUDPServer(t, func([]byte) []byte {
		return smartDiscoveryReplyBytes(`{"error_code":0,"result":{
			"device_type":"SMART.KASAPLUG","device_model":"EP25(US)","alias":"Freezer",
			"mgt_encrypt_schm":{"encrypt_type":"KLAP","http_port":80,"lv":2}
		}}`)
	})
	_, iotPort := newUDPServer(t, func([]byte) []byte { return nil })
	withDiscoveryPorts(t, iotPort, smartPort)

	creds := Credentials{Username: "user@example.com", Password: "secret"}
	devices, err := Discover(t.Context(), "127.0.0.1", 500*time.Millisecond, 1, creds, 2*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := 1, len(devices); want != have {
		t.Fatalf("want %d device, have %d", want, have)
	}

	device := devices[0]
	if want, have := "Freezer", device.Alias(); want != have {
		t.Errorf("want alias %q, have %q", want, have)
	}
	if device.discovered == nil {
		t.Error("want the discovery result carried on the device")
	}
	if device.creds != creds {
		t.Error("want the credentials carried on the device")
	}
	if want, have := 2*time.Second, device.timeout; want != have {
		t.Errorf("want timeout %s, have %s", want, have)
	}
}
