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
	"encoding/binary"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"
)

func TestXOREncryptKnownVector(t *testing.T) {
	// The cipher is fixed by the firmware, so this pins the discovery probe's
	// exact bytes rather than only checking that decrypt undoes encrypt: a
	// round-trip test would pass just as happily on the wrong cipher.
	const want = "d0f281f88bff9af7d5ef94b6d1b4c09fec95e68fe187e8caf08bf68bf6"

	got := hex.EncodeToString(xorEncrypt([]byte(`{"system":{"get_sysinfo":{}}}`)))
	if got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestXORDecryptKnownVector(t *testing.T) {
	// The decrypt side is keyed on the previous ciphertext byte rather than the
	// previous plaintext byte, so it is pinned separately: swapping the two is
	// a mistake that survives an encrypt-only test.
	cipher, err := hex.DecodeString("d0f281f88bff9af7d5ef94b6d1b4c09fec95e68fe187e8caf08bf68bf6")
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if want, have := `{"system":{"get_sysinfo":{}}}`, string(xorDecrypt(cipher)); want != have {
		t.Fatalf("want %q, have %q", want, have)
	}
}

func TestXORRoundTrip(t *testing.T) {
	for _, payload := range []string{"", "a", `{"system":{"get_sysinfo":{}}}`, string(bytes.Repeat([]byte{0}, 300))} {
		if got := string(xorDecrypt(xorEncrypt([]byte(payload)))); got != payload {
			t.Fatalf("round trip of %d bytes returned %q", len(payload), got)
		}
	}
}

func TestXORTransportQuery(t *testing.T) {
	device, address := newXORServer(t, func(request []byte) []byte {
		if want, have := `{"system":{"get_sysinfo":{}}}`, string(request); want != have {
			t.Errorf("want request %q, have %q", want, have)
		}
		return []byte(`{"system":{"get_sysinfo":{"alias":"Rack A"}}}`)
	})

	host, port := splitHostPort(t, address)
	transport := &xorTransport{host: host, port: port, timeout: 2 * time.Second}

	got, err := transport.Query(t.Context(), []byte(`{"system":{"get_sysinfo":{}}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := `{"system":{"get_sysinfo":{"alias":"Rack A"}}}`, string(got); want != have {
		t.Fatalf("want response %q, have %q", want, have)
	}
	if device.queryCount() != 1 {
		t.Fatalf("want 1 query, got %d", device.queryCount())
	}
}

func TestXORTransportRejectsOversizedResponse(t *testing.T) {
	// A device declares its own body length, so a declared length past the cap
	// must be refused rather than allocated.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer func() { _ = listener.Close() }()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		buffer := make([]byte, 1024)
		_, _ = conn.Read(buffer)

		var header [4]byte
		binary.BigEndian.PutUint32(header[:], xorMaxPayload+1)
		_, _ = conn.Write(header[:])
	}()

	host, port := splitHostPort(t, listener.Addr().String())
	transport := &xorTransport{host: host, port: port, timeout: 2 * time.Second}

	if _, err := transport.Query(t.Context(), []byte("{}")); err == nil {
		t.Fatal("expected an error for an oversized declared length")
	}
}

func TestXORTransportRespectsContext(t *testing.T) {
	// A device that accepts the connection and then goes silent must not hold
	// the scrape open past its deadline.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer func() { _ = listener.Close() }()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		// Held open without answering until the test finishes.
		t.Cleanup(func() { _ = conn.Close() })
	}()

	host, port := splitHostPort(t, listener.Addr().String())
	transport := &xorTransport{host: host, port: port, timeout: time.Minute}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := transport.Query(ctx, []byte("{}")); err == nil {
		t.Fatal("expected an error once the context expired")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("query ignored the context deadline, took %s", elapsed)
	}
}

func TestXORTransportUnreachable(t *testing.T) {
	transport := &xorTransport{host: "127.0.0.1", port: 1, timeout: time.Second}
	if _, err := transport.Query(t.Context(), []byte("{}")); err == nil {
		t.Fatal("expected an error connecting to a closed port")
	}
	// Close is a no-op but must stay safe to call on a transport that never
	// connected, since a failed read always closes.
	transport.Close()
}

func TestXORTransportRejectsOversizedRequest(t *testing.T) {
	// The four-byte length prefix is what bounds a request: a length that did
	// not fit would be truncated by the conversion and the device would read
	// the wrong number of bytes off the wire.
	transport := &xorTransport{host: "127.0.0.1", port: 1, timeout: time.Second}

	_, err := transport.Query(t.Context(), make([]byte, xorMaxPayload+1))
	if err == nil {
		t.Fatal("expected an error for a request past the prefix limit")
	}
	if !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("want the limit named in the error, got %q", err)
	}
}
