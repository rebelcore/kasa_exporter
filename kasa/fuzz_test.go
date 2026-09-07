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
	"testing"
)

// The targets here all decode bytes the exporter did not produce. A discovery
// reply is an unsolicited UDP datagram from anywhere on the LAN, and the
// padding and framing routines run on whatever came back through a transport,
// so each is reachable by anything that can put a packet on the network. None
// of them may panic, however malformed the input.

// FuzzParseDiscoveryReply drives the parser for both discovery ports with
// arbitrary datagrams.
func FuzzParseDiscoveryReply(f *testing.F) {
	f.Add(discoveryPortIOT, xorEncrypt([]byte(`{"system":{"get_sysinfo":{"model":"HS300(US)"}}}`)))
	f.Add(discoveryPortIOT, xorEncrypt([]byte(`{}`)))
	f.Add(discoveryPortSMART, append(bytes.Repeat([]byte{0}, discoveryHeaderSMART),
		[]byte(`{"error_code":0,"result":{"device_model":"EP25(US)","mgt_encrypt_schm":{"encrypt_type":"KLAP","lv":2}}}`)...))
	f.Add(discoveryPortSMART, []byte{})
	f.Add(1234, []byte("neither port"))

	f.Fuzz(func(t *testing.T, port int, data []byte) {
		got := parseDiscoveryReply("192.168.1.1", port, data)
		if got == nil {
			return
		}
		// Anything the parser accepts is about to be used to build a
		// connection, so the fields that decide how must stay sane.
		if got.port() <= 0 || got.port() > 65535 {
			t.Fatalf("accepted a reply with an unusable port %d", got.port())
		}
		if v := got.loginVersion(); v != klapLoginV1 && v != klapLoginV2 {
			// Any other value would be handed to the auth hash, which only
			// knows these two.
			t.Logf("login version %d from a fuzzed reply", v)
		}
		_ = got.dialect()
		_ = got.describe()
	})
}

// FuzzXORRoundTrip checks the obfuscation is its own inverse for any input.
func FuzzXORRoundTrip(f *testing.F) {
	f.Add([]byte(`{"system":{"get_sysinfo":{}}}`))
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0}, 512))

	f.Fuzz(func(t *testing.T, data []byte) {
		if got := xorDecrypt(xorEncrypt(data)); !bytes.Equal(got, data) {
			t.Fatalf("round trip changed %d bytes", len(data))
		}
	})
}

// FuzzPKCS7Unpad drives the unpadding on arbitrary plaintext. It runs on the
// output of a block cipher, so the bytes are effectively random whenever a
// session key is wrong.
func FuzzPKCS7Unpad(f *testing.F) {
	f.Add(bytes.Repeat([]byte{16}, 16))
	f.Add(append(bytes.Repeat([]byte{'x'}, 15), 1))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := pkcs7Unpad(data, 16)
		if err != nil {
			return
		}
		if len(out) > len(data) {
			t.Fatalf("unpadding grew the input from %d to %d bytes", len(data), len(out))
		}
	})
}

// FuzzKlapDecrypt drives the KLAP response framing with a fixed session key,
// so the input stands in for whatever a device — or something pretending to be
// one — returned.
func FuzzKlapDecrypt(f *testing.F) {
	f.Add(bytes.Repeat([]byte{0}, klapSigLen+16))
	f.Add(bytes.Repeat([]byte{0}, klapSigLen))
	f.Add([]byte{})

	transport := &klapTransport{
		encKey: bytes.Repeat([]byte{1}, 16),
		sigKey: bytes.Repeat([]byte{2}, 28),
		iv:     bytes.Repeat([]byte{3}, 12),
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		//nolint:errcheck // the point is that it must not panic, whatever it returns.
		_, _ = transport.klapDecrypt(data, 1)
	})
}
