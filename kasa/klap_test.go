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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// klapTestSeeds are the fixed seeds the derivation vectors below were computed
// from, so a mistake in the derivation shows up as a mismatch rather than as
// two wrong values agreeing with each other.
var (
	klapTestLocalSeed  = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	klapTestRemoteSeed = []byte{100, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111, 112, 113, 114, 115}
)

func TestKlapAuthHash(t *testing.T) {
	// Both login versions are pinned against the reference implementation's
	// output. Version 2 is not cosmetic: firmware that wants it rejects a
	// version 1 hash with an error that reads as a wrong password, which is the
	// failure this exporter exists to avoid on an HS300 running hardware 2.0.
	tests := []struct {
		name    string
		creds   Credentials
		version int
		want    string
	}{
		{
			name:    "login version 1",
			creds:   Credentials{Username: "kasa@tp-link.net", Password: "kasaSetup"},
			version: klapLoginV1,
			want:    "74b751bdd208bc155ae2a6d4167cc44d",
		},
		{
			name:    "login version 2",
			creds:   Credentials{Username: "kasa@tp-link.net", Password: "kasaSetup"},
			version: klapLoginV2,
			want:    "49fc4839a2733ffbf464337648f65dc411778d4c03ec82c5e4cccc682163fa64",
		},
		{
			name:    "blank credentials, login version 1",
			creds:   Credentials{},
			version: klapLoginV1,
			want:    "5873dd45edd01f09c1ef2e7819369e8e",
		},
		{
			name:    "blank credentials, login version 2",
			creds:   Credentials{},
			version: klapLoginV2,
			want:    "b8628ab91c74f531603f6d5b45e730e54c123a7247b8978272c03f1e13560cea",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hex.EncodeToString(klapAuthHash(tt.creds, tt.version)); got != tt.want {
				t.Fatalf("want %s, got %s", tt.want, got)
			}
		})
	}
}

func TestKlapDeriveSession(t *testing.T) {
	authHash := klapAuthHash(Credentials{Username: "kasa@tp-link.net", Password: "kasaSetup"}, klapLoginV2)
	key, sig, iv, seq := klapDeriveSession(klapTestLocalSeed, klapTestRemoteSeed, authHash)

	if want, got := "ef8892aee9979567ccf21b7be66dd4f4", hex.EncodeToString(key); got != want {
		t.Errorf("key: want %s, got %s", want, got)
	}
	if want, got := "293dffffe2459b6c62f4b98ca5e88efbc73a74f94d4f5cbf51e40b3a", hex.EncodeToString(sig); got != want {
		t.Errorf("signing key: want %s, got %s", want, got)
	}
	if want, got := "a180241bc357611438f8857d", hex.EncodeToString(iv); got != want {
		t.Errorf("iv: want %s, got %s", want, got)
	}
	// The sequence number is read as a signed 32-bit integer, so a derivation
	// that treats it as unsigned produces a positive value here and every
	// request after the first is encrypted under the wrong IV.
	if want := int32(-1509343370); seq != want {
		t.Errorf("sequence: want %d, got %d", want, seq)
	}
}

func TestKlapTransportQuery(t *testing.T) {
	creds := Credentials{Username: "user@example.com", Password: "secret"}
	device, baseURL := newKLAPServer(t, &klapDevice{
		credentials: creds,
		respond: func(request []byte) []byte {
			if want, have := `{"method":"get_device_info"}`, string(request); want != have {
				t.Errorf("want request %q, have %q", want, have)
			}
			return []byte(`{"error_code":0,"result":{"model":"EP25"}}`)
		},
	})

	transport := &klapTransport{
		client:       httpClient(2 * time.Second),
		baseURL:      baseURL,
		credentials:  creds,
		loginVersion: klapLoginV2,
	}

	got, err := transport.Query(t.Context(), []byte(`{"method":"get_device_info"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := `{"error_code":0,"result":{"model":"EP25"}}`, string(got); want != have {
		t.Fatalf("want %q, have %q", want, have)
	}

	// A second query must reuse the session: re-handshaking on every scrape
	// would triple the round trips a fleet costs.
	if _, err := transport.Query(t.Context(), []byte(`{"method":"get_device_info"}`)); err != nil {
		t.Fatalf("unexpected error on the second query: %v", err)
	}
	if got := device.handshakeCount(); got != 1 {
		t.Fatalf("want 1 handshake across two queries, got %d", got)
	}
}

func TestKlapTransportRehandshakesAfterSessionLoss(t *testing.T) {
	// A device that reboots forgets the session and answers 403. Without a
	// re-handshake every scrape from then on fails until the exporter restarts.
	creds := Credentials{Username: "user@example.com", Password: "secret"}
	device, baseURL := newKLAPServer(t, &klapDevice{
		credentials: creds,
		expireAfter: 1,
		respond:     func([]byte) []byte { return []byte(`{"error_code":0,"result":{}}`) },
	})

	transport := &klapTransport{
		client:       httpClient(2 * time.Second),
		baseURL:      baseURL,
		credentials:  creds,
		loginVersion: klapLoginV2,
	}

	if _, err := transport.Query(t.Context(), []byte(`{}`)); err != nil {
		t.Fatalf("unexpected error on the first query: %v", err)
	}
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err != nil {
		t.Fatalf("unexpected error after the session expired: %v", err)
	}
	if got := device.handshakeCount(); got != 2 {
		t.Fatalf("want 2 handshakes, got %d", got)
	}
}

func TestKlapTransportFallbackCredentials(t *testing.T) {
	// A device that was never bound to a TP-Link account authenticates with
	// blank credentials, and must still be reachable when the operator has
	// configured a login for the rest of the fleet.
	_, baseURL := newKLAPServer(t, &klapDevice{
		credentials: Credentials{},
		respond:     func([]byte) []byte { return []byte(`{"error_code":0,"result":{}}`) },
	})

	transport := &klapTransport{
		client:       httpClient(2 * time.Second),
		baseURL:      baseURL,
		credentials:  Credentials{Username: "someone@example.com", Password: "not-this-device"},
		loginVersion: klapLoginV2,
	}

	if _, err := transport.Query(t.Context(), []byte(`{}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestKlapTransportWrongCredentials(t *testing.T) {
	_, baseURL := newKLAPServer(t, &klapDevice{
		credentials: Credentials{Username: "owner@example.com", Password: "correct"},
		respond:     func([]byte) []byte { return []byte(`{}`) },
	})

	transport := &klapTransport{
		client:       httpClient(2 * time.Second),
		baseURL:      baseURL,
		credentials:  Credentials{Username: "someone@example.com", Password: "wrong"},
		loginVersion: klapLoginV2,
	}

	_, err := transport.Query(t.Context(), []byte(`{}`))
	if !errors.Is(err, errKlapUnauthorized) {
		t.Fatalf("want an unauthorized error, got %v", err)
	}
}

func TestKlapTransportWrongLoginVersion(t *testing.T) {
	// Talking login version 1 to firmware that wants version 2 produces a hash
	// mismatch, which is the failure that looks like a wrong password.
	_, baseURL := newKLAPServer(t, &klapDevice{
		credentials:  Credentials{Username: "owner@example.com", Password: "correct"},
		loginVersion: klapLoginV2,
		respond:      func([]byte) []byte { return []byte(`{}`) },
	})

	transport := &klapTransport{
		client:       httpClient(2 * time.Second),
		baseURL:      baseURL,
		credentials:  Credentials{Username: "owner@example.com", Password: "correct"},
		loginVersion: klapLoginV1,
	}

	if _, err := transport.Query(t.Context(), []byte(`{}`)); !errors.Is(err, errKlapUnauthorized) {
		t.Fatalf("want an unauthorized error, got %v", err)
	}
}

func TestKlapTransportBadHandshakeResponses(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "short handshake1 body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(bytes.Repeat([]byte{1}, 8))
			},
		},
		{
			name: "handshake1 error status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "no", http.StatusInternalServerError)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			transport := &klapTransport{
				client:       httpClient(2 * time.Second),
				baseURL:      srv.URL + "/app",
				loginVersion: klapLoginV2,
			}
			if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestKlapTransportCloseForcesRehandshake(t *testing.T) {
	creds := Credentials{Username: "user@example.com", Password: "secret"}
	device, baseURL := newKLAPServer(t, &klapDevice{
		credentials: creds,
		respond:     func([]byte) []byte { return []byte(`{"error_code":0,"result":{}}`) },
	})

	transport := &klapTransport{
		client:       httpClient(2 * time.Second),
		baseURL:      baseURL,
		credentials:  creds,
		loginVersion: klapLoginV2,
	}

	if _, err := transport.Query(t.Context(), []byte(`{}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	transport.Close()
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err != nil {
		t.Fatalf("unexpected error after close: %v", err)
	}
	if got := device.handshakeCount(); got != 2 {
		t.Fatalf("want 2 handshakes, got %d", got)
	}
}

func TestKlapTransportCancelledContext(t *testing.T) {
	_, baseURL := newKLAPServer(t, &klapDevice{
		credentials: Credentials{},
		respond:     func([]byte) []byte { return []byte(`{}`) },
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	transport := &klapTransport{
		client:       httpClient(2 * time.Second),
		baseURL:      baseURL,
		loginVersion: klapLoginV2,
	}
	if _, err := transport.Query(ctx, []byte(`{}`)); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

func TestPKCS7(t *testing.T) {
	for _, size := range []int{0, 1, 15, 16, 17, 64} {
		data := bytes.Repeat([]byte{'x'}, size)
		padded := pkcs7Pad(append([]byte(nil), data...), 16)
		if len(padded)%16 != 0 || len(padded) <= size {
			t.Fatalf("padding %d bytes produced %d", size, len(padded))
		}
		got, err := pkcs7Unpad(padded, 16)
		if err != nil {
			t.Fatalf("unpadding %d bytes: %v", size, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("round trip of %d bytes returned %q", size, got)
		}
	}
}

func TestPKCS7UnpadRejectsBadPadding(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "not a block multiple", data: bytes.Repeat([]byte{1}, 17)},
		{name: "zero length byte", data: append(bytes.Repeat([]byte{1}, 15), 0)},
		{name: "length over the block size", data: append(bytes.Repeat([]byte{1}, 15), 17)},
		{name: "inconsistent pad bytes", data: append(bytes.Repeat([]byte{1}, 14), 2, 3)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := pkcs7Unpad(tt.data, 16); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
