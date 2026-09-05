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
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// xorKey seeds TP-Link's autokey cipher. Every IOT-family device ships with the
// same constant, which is why the "encryption" on port 9999 is obfuscation
// rather than security: anyone on the LAN can read the traffic.
const xorKey byte = 0xAB

// xorPort is the TCP/UDP port the legacy IOT protocol listens on.
const xorPort = 9999

// xorMaxPayload caps how large a response the XOR transport will accept. The
// device declares the body length itself, so without a ceiling a malfunctioning
// or hostile device could ask the exporter to allocate an arbitrary buffer. The
// largest legitimate payload here is an HS300's sysinfo with six children,
// comfortably under 100 KiB.
const xorMaxPayload = 1 << 20 // 1 MiB

// xorEncrypt applies TP-Link's autokey stream cipher: each output byte is the
// plaintext byte XORed with the previous *ciphertext* byte, seeded with
// xorKey. It is its own protocol, not a standard cipher, so encrypt and
// decrypt are written out separately rather than sharing one routine.
func xorEncrypt(plain []byte) []byte {
	out := make([]byte, len(plain))
	key := xorKey
	for i, b := range plain {
		key = b ^ key
		out[i] = key
	}
	return out
}

// xorDecrypt reverses xorEncrypt: each plaintext byte is the ciphertext byte
// XORed with the previous ciphertext byte, seeded with xorKey.
func xorDecrypt(cipher []byte) []byte {
	out := make([]byte, len(cipher))
	key := xorKey
	for i, c := range cipher {
		out[i] = c ^ key
		key = c
	}
	return out
}

// xorTransport talks the legacy IOT protocol over TCP port 9999.
//
// A fresh connection is opened for every query rather than held open between
// scrapes. The devices drop idle sockets without notice, so a cached connection
// mostly serves to turn the *next* scrape into a failure; reconnecting costs
// one round trip on a LAN and is what makes an HS300 survive a reboot without
// operator intervention.
type xorTransport struct {
	host string
	// port is the device's control port. It is only ever anything but 9999 in
	// tests, where the fake device cannot bind a fixed one.
	port    int
	timeout time.Duration
}

// Query sends one JSON request and returns the JSON response body.
func (t *xorTransport) Query(ctx context.Context, request []byte) ([]byte, error) {
	port := t.port
	if port == 0 {
		port = xorPort
	}

	dialer := net.Dialer{Timeout: t.timeout}
	addr := net.JoinHostPort(t.host, fmt.Sprint(port))
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	// A deadline derived from the context keeps a device that accepts the
	// connection and then goes silent from holding the scrape open: the
	// TCP-level timeout only covers the dial.
	deadline := time.Now().Add(t.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	// On TCP the payload carries a four-byte big-endian length prefix; on UDP
	// (discovery) the same ciphertext is sent bare.
	frame := make([]byte, 4+len(request))
	binary.BigEndian.PutUint32(frame, uint32(len(request)))
	copy(frame[4:], xorEncrypt(request))
	if _, err := conn.Write(frame); err != nil {
		return nil, fmt.Errorf("sending to %s: %w", addr, err)
	}

	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, fmt.Errorf("reading length from %s: %w", addr, err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 {
		return nil, fmt.Errorf("empty response from %s", addr)
	}
	if length > xorMaxPayload {
		return nil, fmt.Errorf("response from %s declares %d bytes, over the %d byte limit", addr, length, xorMaxPayload)
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", addr, err)
	}
	return xorDecrypt(body), nil
}

// Close is a no-op: the transport holds no connection between queries.
func (t *xorTransport) Close() {}
