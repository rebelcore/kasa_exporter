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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/version"
)

// This file covers the failure branches that the happy-path tests do not reach:
// truncated responses, malformed payloads and the recovery paths each of them
// is supposed to take.

func TestUserAgentIncludesTheBuildVersion(t *testing.T) {
	// Operators use this to tell which exporter, and which release, is querying
	// their devices.
	prev := version.Version
	t.Cleanup(func() { version.Version = prev })

	version.Version = ""
	if want, have := "kasa_exporter", userAgent(); want != have {
		t.Errorf("want %q with no version stamped in, have %q", want, have)
	}

	version.Version = "1.2.3"
	if want, have := "kasa_exporter/1.2.3", userAgent(); want != have {
		t.Errorf("want %q, have %q", want, have)
	}
}

func TestXORTransportTruncatedResponses(t *testing.T) {
	tests := []struct {
		name  string
		write func(conn net.Conn)
	}{
		{
			// The device accepts the connection and then goes away before
			// answering at all.
			name:  "no response",
			write: func(net.Conn) {},
		},
		{
			// A length header with no body behind it: reading the declared
			// number of bytes must fail rather than block or return rubbish.
			name: "header without a body",
			write: func(conn net.Conn) {
				var header [4]byte
				binary.BigEndian.PutUint32(header[:], 64)
				_, _ = conn.Write(header[:])
			},
		},
		{
			name: "zero length",
			write: func(conn net.Conn) {
				_, _ = conn.Write([]byte{0, 0, 0, 0})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
				buffer := make([]byte, 1024)
				_, _ = conn.Read(buffer)
				tt.write(conn)
				_ = conn.Close()
			}()

			host, port := splitHostPort(t, listener.Addr().String())
			transport := &xorTransport{host: host, port: port, timeout: 2 * time.Second}

			if _, err := transport.Query(t.Context(), []byte("{}")); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestXORTransportDefaultsToPort9999(t *testing.T) {
	// A transport built without an explicit port must address the device's real
	// control port, not port zero.
	transport := &xorTransport{host: "127.0.0.1", timeout: 50 * time.Millisecond}
	_, err := transport.Query(t.Context(), []byte("{}"))
	if err == nil {
		t.Fatal("expected an error connecting to a port with no device on it")
	}
	if !strings.Contains(err.Error(), "9999") {
		t.Fatalf("want the error to name port 9999, got %q", err)
	}
}

func TestKlapPostRejectsAnInvalidURL(t *testing.T) {
	transport := &klapTransport{client: httpClient(time.Second), baseURL: "://not a url"}
	if _, _, _, err := transport.klapPost(t.Context(), "/handshake1", nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestKlapDecryptRejectsMalformedBodies(t *testing.T) {
	transport := &klapTransport{
		encKey: bytes.Repeat([]byte{1}, 16),
		sigKey: bytes.Repeat([]byte{2}, 28),
		iv:     bytes.Repeat([]byte{3}, 12),
	}

	tests := []struct {
		name string
		body []byte
	}{
		{name: "shorter than a signature", body: bytes.Repeat([]byte{0}, klapSigLen)},
		{name: "not a block multiple", body: bytes.Repeat([]byte{0}, klapSigLen+5)},
		{name: "not valid padding", body: bytes.Repeat([]byte{0}, klapSigLen+16)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := transport.klapDecrypt(tt.body, 1); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestKlapQueryUnexpectedStatus(t *testing.T) {
	// A status that is neither success nor an expired session is a real
	// failure, not something to re-handshake over.
	creds := Credentials{Username: "user@example.com", Password: "secret"}
	// The device handshakes normally; only the request path misbehaves, so the
	// failure is isolated to the branch under test.
	device := &klapDevice{t: t, credentials: creds, loginVersion: klapLoginV2, respond: func([]byte) []byte { return nil }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/request") {
			http.Error(w, "teapot", http.StatusTeapot)
			return
		}
		device.ServeHTTP(w, r)
	}))
	defer srv.Close()

	transport := &klapTransport{
		client:       httpClient(2 * time.Second),
		baseURL:      srv.URL + "/app",
		credentials:  creds,
		loginVersion: klapLoginV2,
	}

	_, err := transport.Query(t.Context(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "418") {
		t.Fatalf("want the status in the error, got %q", err)
	}
}

func TestKlapQueryGivesUpAfterTwoRejections(t *testing.T) {
	// A device that rejects the session every time — re-provisioned, or talking
	// to something that is not the device we handshook with — must fail rather
	// than loop.
	creds := Credentials{Username: "user@example.com", Password: "secret"}
	device := &klapDevice{credentials: creds, respond: func([]byte) []byte { return nil }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/request") {
			http.Error(w, "gone", http.StatusForbidden)
			return
		}
		device.ServeHTTP(w, r)
	}))
	defer srv.Close()
	device.t = t
	device.loginVersion = klapLoginV2

	transport := &klapTransport{
		client:       httpClient(2 * time.Second),
		baseURL:      srv.URL + "/app",
		credentials:  creds,
		loginVersion: klapLoginV2,
	}

	if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("expected an error")
	}
	if got := device.handshakeCount(); got != 2 {
		t.Fatalf("want exactly 2 handshakes before giving up, got %d", got)
	}
}

func TestKlapHandshake2Rejected(t *testing.T) {
	// The second post can fail on its own — a device that dropped the session
	// between the two halves of the handshake.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/handshake1"):
			// A hash that only blank credentials can match, so handshake 1
			// succeeds and the failure is isolated to handshake 2.
			body, _ := readAll(r)
			remote := bytes.Repeat([]byte{7}, klapSeedLen)
			auth := klapAuthHash(Credentials{}, klapLoginV2)
			_, _ = w.Write(append(remote, klapSHA256(body, remote, auth)...))
		default:
			http.Error(w, "no", http.StatusForbidden)
		}
	}))
	defer srv.Close()

	transport := &klapTransport{client: httpClient(time.Second), baseURL: srv.URL + "/app", loginVersion: klapLoginV2}
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAESPassthroughMalformedResponses(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "passthrough result is not json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, map[string]any{"error_code": 0, "result": "not an object"})
			},
		},
		{
			name: "response is not base64",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, map[string]any{"error_code": 0, "result": map[string]any{"response": "!!!"}})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A device that handshakes properly and then answers rubbish, so
			// the failure is in the passthrough rather than the handshake.
			device := &aesDevice{acceptLogin: true, respond: func([]byte) []byte { return []byte(`{"error_code":0}`) }}
			var handshaken bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !handshaken {
					handshaken = true
					device.ServeHTTP(w, r)
					return
				}
				tt.handler(w, r)
			}))
			defer srv.Close()
			device.t = t

			transport := &aesTransport{client: httpClient(time.Second), baseURL: srv.URL + "/app"}
			if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestAESHandshakeWrongSecretLength(t *testing.T) {
	// The RSA-wrapped secret is a 16-byte key followed by a 16-byte IV.
	// Anything else cannot produce a session, and must be rejected rather than
	// sliced into whatever happens to be there.
	_, baseURL := newAESServer(t, &aesDevice{
		acceptLogin:  true,
		secretLength: 20,
		respond:      func([]byte) []byte { return nil },
	})

	transport := &aesTransport{client: httpClient(time.Second), baseURL: baseURL}
	_, err := transport.Query(t.Context(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "session key is 20 bytes") {
		t.Fatalf("want the length named in the error, got %q", err)
	}
}

func TestAESHandshakeMalformedResult(t *testing.T) {
	// The handshake result must be an object carrying the wrapped key.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"error_code": 0, "result": "not an object"})
	}))
	defer srv.Close()

	transport := &aesTransport{client: httpClient(time.Second), baseURL: srv.URL + "/app"}
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAESHandshakeKeyIsNotBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"error_code": 0, "result": map[string]any{"key": "!!!"}})
	}))
	defer srv.Close()

	transport := &aesTransport{client: httpClient(time.Second), baseURL: srv.URL + "/app"}
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAESLoginReplyWithoutAToken(t *testing.T) {
	// Both login shapes are tried; a device that answers neither with a token
	// has refused the login however friendly the error code looks.
	tests := []struct {
		name  string
		reply []byte
	}{
		{name: "no token", reply: []byte(`{"error_code":0,"result":{}}`)},
		{name: "not json", reply: []byte(`not json`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, baseURL := newAESServer(t, &aesDevice{
				acceptLogin: true,
				loginReply:  tt.reply,
				respond:     func([]byte) []byte { return nil },
			})

			transport := &aesTransport{client: httpClient(time.Second), baseURL: baseURL}
			if _, err := transport.Query(t.Context(), []byte(`{}`)); !errors.Is(err, errAESUnauthorized) {
				t.Fatalf("want an unauthorized error, got %v", err)
			}
		})
	}
}

func TestAESInnerResponseIsNotJSON(t *testing.T) {
	// The ciphertext decrypted cleanly but is not a reply the transport can
	// read, which is a failure rather than an empty result.
	_, baseURL := newAESServer(t, &aesDevice{
		acceptLogin: true,
		respond:     func([]byte) []byte { return []byte(`not json`) },
	})

	transport := &aesTransport{client: httpClient(time.Second), baseURL: baseURL}
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAESSessionKeyNotEncryptedToUs(t *testing.T) {
	// Something on the device's address that is not the device: the wrapped key
	// does not open with the key pair generated for this handshake.
	_, baseURL := newAESServer(t, &aesDevice{
		acceptLogin:    true,
		unwrappableKey: true,
		respond:        func([]byte) []byte { return nil },
	})

	transport := &aesTransport{client: httpClient(time.Second), baseURL: baseURL}
	_, err := transport.Query(t.Context(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "decrypting the session key") {
		t.Fatalf("want the handshake failure named, got %q", err)
	}
}

func TestAESLoginReplyOfTheWrongShape(t *testing.T) {
	// Valid JSON at the envelope level, but the result is not an object with a
	// token in it. Both login shapes are tried and both are refused.
	_, baseURL := newAESServer(t, &aesDevice{
		acceptLogin: true,
		loginReply:  []byte(`{"error_code":0,"result":[]}`),
		respond:     func([]byte) []byte { return nil },
	})

	transport := &aesTransport{client: httpClient(time.Second), baseURL: baseURL}
	if _, err := transport.Query(t.Context(), []byte(`{}`)); !errors.Is(err, errAESUnauthorized) {
		t.Fatalf("want an unauthorized error, got %v", err)
	}
}

func TestAESResponseCannotBeDecrypted(t *testing.T) {
	// The session is established but the reply is not ciphertext this session
	// can open, which must fail rather than be reported as an empty reading.
	_, baseURL := newAESServer(t, &aesDevice{
		acceptLogin:       true,
		corruptCiphertext: true,
		respond:           func([]byte) []byte { return []byte(`{"error_code":0}`) },
	})

	transport := &aesTransport{client: httpClient(time.Second), baseURL: baseURL}
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAESGivesUpWhenEveryRequestExpires(t *testing.T) {
	// The login works but every request comes back expired, so the one retry is
	// used up and the query fails instead of looping.
	device, baseURL := newAESServer(t, &aesDevice{
		acceptLogin:        true,
		expireEveryRequest: true,
		respond:            func([]byte) []byte { return nil },
	})

	transport := &aesTransport{client: httpClient(time.Second), baseURL: baseURL}
	_, err := transport.Query(t.Context(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "expired twice") {
		t.Fatalf("want the give-up message, got %q", err)
	}
	if got := device.handshakeCount(); got != 2 {
		t.Fatalf("want exactly 2 handshakes before giving up, got %d", got)
	}
}

func TestAESNetworkFailureDuringPassthrough(t *testing.T) {
	// The session was established and then the device went away.
	device := &aesDevice{t: t, acceptLogin: true,
		respond: func([]byte) []byte { return []byte(`{"error_code":0,"result":{}}`) }}
	srv := httptest.NewServer(device)

	transport := &aesTransport{
		client:  &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}},
		baseURL: srv.URL + "/app",
	}
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	srv.Close()
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("expected an error once the device went away")
	}
}

func TestDiscoverHostSurfacesASweepFailure(t *testing.T) {
	if _, err := discoverHost(t.Context(), "no-such-host.invalid", 100*time.Millisecond); err == nil {
		t.Fatal("want an error for a target that does not resolve")
	}
}

func TestAESPostRejectsAnInvalidURL(t *testing.T) {
	transport := &aesTransport{client: httpClient(time.Second), baseURL: "://not a url"}
	if _, err := transport.post(t.Context(), "", []byte(`{}`)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestTransportsRejectOversizedBodies(t *testing.T) {
	// The device declares its own body length, so both HTTP transports cap what
	// they will read. The cap is far above any real reply, so it is shrunk here
	// rather than sending eight megabytes.
	prev := maxResponseBytes
	maxResponseBytes = 8
	t.Cleanup(func() { maxResponseBytes = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, 64))
	}))
	defer srv.Close()

	t.Run("klap", func(t *testing.T) {
		transport := &klapTransport{client: httpClient(time.Second), baseURL: srv.URL + "/app", loginVersion: klapLoginV2}
		_, _, _, err := transport.klapPost(t.Context(), "/handshake1", nil)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("want the limit named in the error, got %q", err)
		}
	})

	t.Run("aes", func(t *testing.T) {
		transport := &aesTransport{client: httpClient(time.Second), baseURL: srv.URL + "/app"}
		_, err := transport.post(t.Context(), "", []byte(`{}`))
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("want the limit named in the error, got %q", err)
		}
	})
}

func TestKlapNetworkFailureAfterHandshake1(t *testing.T) {
	// The device answered the first half of the handshake and then went away.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	srv := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := readAll(r)
			remote := bytes.Repeat([]byte{7}, klapSeedLen)
			auth := klapAuthHash(Credentials{}, klapLoginV2)
			_, _ = w.Write(append(remote, klapSHA256(body, remote, auth)...))
			// Stop listening so the next request has nowhere to go.
			go func() { _ = listener.Close() }()
		})},
	}
	srv.Start()
	defer srv.Close()

	transport := &klapTransport{
		client:       &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}},
		baseURL:      srv.URL + "/app",
		loginVersion: klapLoginV2,
	}
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestKlapNetworkFailureOnARequest(t *testing.T) {
	// The session was established and then the device went away, which must
	// surface as an error rather than a hang or a stale reading.
	creds := Credentials{Username: "user@example.com", Password: "secret"}
	device := &klapDevice{t: t, credentials: creds, loginVersion: klapLoginV2,
		respond: func([]byte) []byte { return []byte(`{"error_code":0,"result":{}}`) }}
	srv := httptest.NewServer(device)

	transport := &klapTransport{
		client:       &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}},
		baseURL:      srv.URL + "/app",
		credentials:  creds,
		loginVersion: klapLoginV2,
	}
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	srv.Close()
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("expected an error once the device went away")
	}
}

func TestIOTBulbAlreadyOnTheAlternateModule(t *testing.T) {
	// A bulb whose remembered module stops answering falls back to the other
	// name rather than silently losing its energy series for good.
	transport := &stubTransport{
		replies: map[string]string{
			"get_sysinfo": `{"system":{"get_sysinfo":{"model":"KL130(US)","mic_type":"IOT.SMARTBULB","alias":"Hall","err_code":0}},
				"smartlife.iot.common.emeter":{"err_code":-1}}`,
			`{"emeter"`: `{"emeter":{"get_realtime":{"power_mw":7000,"err_code":0}}}`,
		},
	}

	querier := newIOTQuerier(transport)
	querier.emeterModule = iotBulbEmeterModule

	reading, err := querier.Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reading.Energy == nil || *reading.Energy.PowerWatts != 7 {
		t.Fatalf("want 7 W from the other module, have %v", reading.Energy)
	}
	if want, have := iotEmeterModule, querier.emeterModule; want != have {
		t.Errorf("want the module switched to %q, have %q", want, have)
	}
}

func TestAESDecryptRejectsMalformedCiphertext(t *testing.T) {
	transport := &aesTransport{key: bytes.Repeat([]byte{1}, 16), iv: bytes.Repeat([]byte{2}, 16)}

	for _, payload := range [][]byte{nil, bytes.Repeat([]byte{0}, 5), bytes.Repeat([]byte{0}, 16)} {
		if _, err := transport.decrypt(payload); err == nil {
			t.Fatalf("want an error for %d bytes", len(payload))
		}
	}
}

func TestAESQueryGivesUpAfterTwoExpiries(t *testing.T) {
	// Every exchange reports an expired session, so the one retry the transport
	// allows is used up and the query fails rather than looping.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"error_code": 9999})
	}))
	defer srv.Close()

	transport := &aesTransport{client: httpClient(time.Second), baseURL: srv.URL + "/app"}
	if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAESPostRejectsBadStatusAndBody(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "non-200",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "no", http.StatusBadGateway)
			},
		},
		{
			name: "not json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("nope"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			transport := &aesTransport{client: httpClient(time.Second), baseURL: srv.URL + "/app"}
			if _, err := transport.post(t.Context(), "", []byte(`{}`)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestIOTEnergySpeltAsEnergy(t *testing.T) {
	// The last of the four spellings the firmware uses for a cumulative total.
	var raw iotEmeter
	if err := json.Unmarshal([]byte(`{"power":10,"energy":3.5}`), &raw); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	got := raw.energy()
	if got == nil || *got.TotalKWh != 3.5 {
		t.Fatalf("want 3.5 kWh, have %v", got)
	}
}

func TestIOTRetryEmeterSurvivesATransientFailure(t *testing.T) {
	// A network error while probing the alternate module must not permanently
	// mark the device as having no meter: the next scrape should try again.
	transport := &stubTransport{
		replies: map[string]string{
			"get_sysinfo": `{"system":{"get_sysinfo":{"model":"KL130(US)","mic_type":"IOT.SMARTBULB","alias":"Hall","err_code":0}},"emeter":{"err_code":-1}}`,
		},
	}

	querier := newIOTQuerier(transport)
	reading, err := querier.Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reading.Energy != nil {
		t.Errorf("want no energy for this scrape, have %v", reading.Energy)
	}
	if !querier.deviceHasEmeter {
		t.Error("want the meter kept after a transient failure, not written off")
	}
}

func TestIOTStripWithNoChildren(t *testing.T) {
	// A strip that reports no outlets has nothing to read; that is not a
	// failure, just an empty strip.
	transport := &stubTransport{
		fallback: `{"system":{"get_sysinfo":{"model":"KP303(UK)","type":"IOT.SMARTPLUGSWITCH","alias":"Bench","err_code":0}}}`,
	}

	reading, err := newIOTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := TypeStrip, reading.Type; want != have {
		t.Fatalf("want type %q, have %q", want, have)
	}
	if len(reading.Sockets) != 0 {
		t.Fatalf("want no sockets, have %d", len(reading.Sockets))
	}
	if reading.Energy != nil {
		t.Errorf("want no derived energy, have %v", reading.Energy)
	}
}

func TestAddEnergyIgnoresNothing(t *testing.T) {
	// An outlet that reported nothing contributes nothing, rather than zeroing
	// the strip's running total.
	total := &Energy{PowerWatts: float(10)}
	addEnergy(total, nil)
	if *total.PowerWatts != 10 {
		t.Fatalf("want the total untouched, have %g", *total.PowerWatts)
	}
}

func TestIsPrintable(t *testing.T) {
	// This is what separates a base64-encoded name from a short plaintext one
	// that happens to decode: the decoded bytes have to read as text.
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{name: "text", data: []byte("Rack A"), want: true},
		{name: "utf-8", data: []byte("台所"), want: true},
		{name: "empty", data: nil},
		{name: "invalid utf-8", data: []byte{0xff, 0xfe}},
		// Valid UTF-8 but not a name: this is what "Hall" decodes to.
		{name: "control characters", data: []byte("a\x01b")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPrintable(tt.data); got != tt.want {
				t.Fatalf("want %v, got %v", tt.want, got)
			}
		})
	}
}

func TestSMARTBatchFallbackFailure(t *testing.T) {
	// The batch came back without device information and the single-request
	// retry failed too, so there is nothing to report.
	transport := &stubTransport{
		replies: map[string]string{"multipleRequest": `{"error_code":0,"result":{"responses":[]}}`},
	}
	if _, err := newSMARTQuerier(transport).Read(t.Context()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestSMARTRejectsMalformedDeviceInfo(t *testing.T) {
	transport := &stubTransport{
		fallback: `{"error_code":0,"result":{"responses":[{"method":"get_device_info","error_code":0,"result":"not an object"}]}}`,
	}
	if _, err := newSMARTQuerier(transport).Read(t.Context()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestSMARTChildListEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		children    string
		wantSockets int
	}{
		{
			name:        "not an object",
			children:    `"nope"`,
			wantSockets: 0,
		},
		{
			name:        "empty list",
			children:    `{"child_device_list":[]}`,
			wantSockets: 0,
		},
		{
			// Firmware that omits the position falls back to the order the
			// outlets were listed in.
			name:        "no position",
			children:    `{"child_device_list":[{"device_id":"A","nickname":"TkFT"},{"device_id":"B","nickname":"U3dpdGNo"}]}`,
			wantSockets: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &stubTransport{fallback: `{"error_code":0,"result":{"responses":[
				{"method":"get_device_info","error_code":0,"result":{"model":"P300","type":"SMART.TAPOPLUG","nickname":"U3RyaXA="}},
				{"method":"get_child_device_list","error_code":0,"result":` + tt.children + `}
			]}}`}

			reading, err := newSMARTQuerier(transport).Read(t.Context())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if want, have := tt.wantSockets, len(reading.Sockets); want != have {
				t.Fatalf("want %d sockets, have %d", want, have)
			}
			if tt.wantSockets == 2 && reading.Sockets[1].ID != "2" {
				t.Fatalf("want the second outlet numbered 2, have %q", reading.Sockets[1].ID)
			}
		})
	}
}

func TestSMARTUnrecognisedModelWithChildrenIsAStrip(t *testing.T) {
	// A model the exporter does not know, reporting children, is a strip: only
	// a hub is identified by model, and nothing else in the range has children.
	transport := &stubTransport{fallback: `{"error_code":0,"result":{"responses":[
		{"method":"get_device_info","error_code":0,"result":{"model":"ZZ999","type":"SOMETHING.ELSE","nickname":"TmV3"}},
		{"method":"get_child_device_list","error_code":0,"result":{"child_device_list":[{"device_id":"A","position":0,"device_on":true}]}}
	]}}`}

	reading, err := newSMARTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := TypeStrip, reading.Type; want != have {
		t.Fatalf("want type %q, have %q", want, have)
	}
	if want, have := 1, len(reading.Sockets); want != have {
		t.Fatalf("want %d socket, have %d", want, have)
	}
}

func TestSMARTReportsTheStatusLED(t *testing.T) {
	// Reported as led_off, so it is inverted to read as "the LED is lit".
	transport := &stubTransport{fallback: `{"error_code":0,"result":{"responses":[
		{"method":"get_device_info","error_code":0,"result":{"model":"EP25","type":"SMART.KASAPLUG","led_off":1}}
	]}}`}

	reading, err := newSMARTQuerier(transport).Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reading.LEDOn == nil || *reading.LEDOn {
		t.Fatalf("want the LED reported as off, have %v", reading.LEDOn)
	}
}

func TestSMARTQueryRejectsADeviceError(t *testing.T) {
	transport := &stubTransport{fallback: `{"error_code":-1301,"result":null}`}
	if _, err := newSMARTQuerier(transport).query(t.Context(), smartRequest("get_device_info", nil)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDeviceReadNegotiatesOnFirstUse(t *testing.T) {
	// A device with no connection yet negotiates on the first read, and a
	// device that answers nothing fails there rather than silently reporting.
	device := Connect("127.0.0.1", Credentials{}, 100*time.Millisecond)
	if _, err := device.Read(t.Context()); err == nil {
		t.Fatal("expected an error for a device that answers nothing")
	}
	if device.querier != nil {
		t.Error("want no connection kept after a failed negotiation")
	}
}

func TestNegotiateUsesADiscoveryResultDirectly(t *testing.T) {
	// Discovery already said what the device speaks, so exactly one connection
	// is built rather than probing every transport in turn.
	_, address := newXORServer(t, func([]byte) []byte {
		return []byte(`{"system":{"get_sysinfo":{"model":"HS110(US)","alias":"Freezer","err_code":0}},"emeter":{"err_code":-1}}`)
	})
	host, _ := splitHostPort(t, address)

	querier, err := negotiate(t.Context(), host, Credentials{}, time.Second, &discoveryResult{EncryptType: encryptXOR})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := querier.(*iotQuerier); !ok {
		t.Fatalf("want an IOT dialect, got %T", querier)
	}
}

// newDiscoverableKLAPDevice starts a KLAP device together with a discovery
// responder that advertises it, so the path a configured host actually takes —
// ask the device what it speaks, then connect that way — can be driven end to
// end on loopback.
func newDiscoverableKLAPDevice(t *testing.T, creds Credentials, reply string) {
	t.Helper()

	device := &klapDevice{t: t, credentials: creds, loginVersion: klapLoginV2,
		respond: func([]byte) []byte { return []byte(reply) }}
	srv := httptest.NewServer(device)
	t.Cleanup(srv.Close)

	_, klapPort := splitHostPort(t, strings.TrimPrefix(srv.URL, "http://"))

	_, smartPort := newUDPServer(t, func([]byte) []byte {
		return smartDiscoveryReplyBytes(fmt.Sprintf(`{"error_code":0,"result":{
			"device_type":"SMART.KASAPLUG","device_model":"EP25(US)","alias":"Freezer",
			"mgt_encrypt_schm":{"encrypt_type":"KLAP","http_port":%d,"lv":2}
		}}`, klapPort))
	})
	_, iotPort := newUDPServer(t, func([]byte) []byte { return nil })
	withDiscoveryPorts(t, iotPort, smartPort)
}

func TestNegotiateAsksTheDeviceFirst(t *testing.T) {
	// A configured host that never answered a broadcast is still asked what it
	// speaks before anything is guessed, because that answer removes the
	// guesswork entirely.
	creds := Credentials{Username: "user@example.com", Password: "secret"}
	newDiscoverableKLAPDevice(t, creds, `{"error_code":0,"result":{"responses":[
		{"method":"get_device_info","error_code":0,"result":{"model":"EP25","type":"SMART.KASAPLUG","nickname":"RnJlZXplcg==","device_on":true}}
	]}}`)

	querier, err := negotiate(t.Context(), "127.0.0.1", creds, 2*time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer querier.Close()

	reading, err := querier.Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := "Freezer", reading.Alias; want != have {
		t.Fatalf("want alias %q, have %q", want, have)
	}
}

func TestDeviceReadConnectsOnFirstUse(t *testing.T) {
	// The connection is established lazily and then kept, so a fleet pays for
	// each handshake once rather than once per scrape.
	creds := Credentials{Username: "user@example.com", Password: "secret"}
	newDiscoverableKLAPDevice(t, creds, `{"error_code":0,"result":{"responses":[
		{"method":"get_device_info","error_code":0,"result":{"model":"EP25","type":"SMART.KASAPLUG","nickname":"RnJlZXplcg==","device_on":true}}
	]}}`)

	device := Connect("127.0.0.1", creds, 2*time.Second)
	defer device.Close()

	reading, err := device.Read(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := TypePlug, reading.Type; want != have {
		t.Fatalf("want type %q, have %q", want, have)
	}
	if device.querier == nil {
		t.Fatal("want the connection kept for the next scrape")
	}

	if _, err := device.Read(t.Context()); err != nil {
		t.Fatalf("unexpected error on the second read: %v", err)
	}
}

func TestNegotiateDiscardsAConnectionThatCannotRead(t *testing.T) {
	// Discovery answered, but the device it pointed at does not work. The
	// half-built connection must be closed rather than dropped, since it may
	// already hold a session, and the probe order tried afterwards.
	_, smartPort := newUDPServer(t, func([]byte) []byte {
		// Advertises a port with nothing listening on it.
		return smartDiscoveryReplyBytes(`{"error_code":0,"result":{
			"device_type":"SMART.KASAPLUG","device_model":"EP25(US)",
			"mgt_encrypt_schm":{"encrypt_type":"KLAP","http_port":1,"lv":2}
		}}`)
	})
	_, iotPort := newUDPServer(t, func([]byte) []byte { return nil })
	withDiscoveryPorts(t, iotPort, smartPort)

	if _, err := negotiate(t.Context(), "127.0.0.1", Credentials{}, 200*time.Millisecond, nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDiscoverSurfacesASweepFailure(t *testing.T) {
	if _, err := Discover(t.Context(), "no-such-host.invalid", 100*time.Millisecond, 1, Credentials{}, time.Second); err == nil {
		t.Fatal("want an error for a target that does not resolve")
	}
}

func TestUnicastDiscoveryBudget(t *testing.T) {
	// Regression: asking a device what it speaks used to be handed the whole
	// device timeout. Against a real HS300 that answers discovery on 20002 but
	// has 9999 closed, the question consumed the entire budget and every
	// transport afterwards failed with "context deadline exceeded" before it
	// had been tried at all.
	tests := []struct {
		timeout time.Duration
		want    time.Duration
	}{
		{timeout: 8 * time.Second, want: 2 * time.Second},
		{timeout: 5 * time.Second, want: 1250 * time.Millisecond},
		// Short timeouts get the floor rather than something no device could
		// answer within.
		{timeout: time.Second, want: 500 * time.Millisecond},
		// ...but never more than the caller allowed.
		{timeout: 200 * time.Millisecond, want: 200 * time.Millisecond},
	}

	for _, tt := range tests {
		if got := unicastDiscoveryBudget(tt.timeout); got != tt.want {
			t.Errorf("timeout %s: want %s, got %s", tt.timeout, tt.want, got)
		}
		if got := unicastDiscoveryBudget(tt.timeout); got > tt.timeout {
			t.Errorf("timeout %s: budget %s exceeds it", tt.timeout, got)
		}
	}
}

func TestNegotiateLeavesTimeForTheConnection(t *testing.T) {
	// The other half of that regression: a device that does not answer the
	// question must still leave enough of the budget for the transports that
	// follow, rather than having them all fail on an expired context.
	_, iotPort := newUDPServer(t, func([]byte) []byte { return nil })
	_, smartPort := newUDPServer(t, func([]byte) []byte { return nil })
	withDiscoveryPorts(t, iotPort, smartPort)

	// A KLAP device on a loopback port, reached through the probe order rather
	// than through discovery.
	creds := Credentials{Username: "user@example.com", Password: "secret"}
	device := &klapDevice{t: t, credentials: creds, loginVersion: klapLoginV2,
		respond: func([]byte) []byte {
			return []byte(`{"error_code":0,"result":{"responses":[
				{"method":"get_device_info","error_code":0,"result":{"model":"EP25","type":"SMART.KASAPLUG","device_on":true}}
			]}}`)
		}}
	srv := httptest.NewServer(device)
	defer srv.Close()
	_, klapPort := splitHostPort(t, strings.TrimPrefix(srv.URL, "http://"))

	prevOrder := probeOrder
	probeOrder = func() []*discoveryResult {
		return []*discoveryResult{
			{EncryptType: encryptKLAP, Family: "SMART", LoginVersion: klapLoginV2, HTTPPort: klapPort},
		}
	}
	t.Cleanup(func() { probeOrder = prevOrder })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	querier, err := negotiate(ctx, "127.0.0.1", creds, 2*time.Second, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer querier.Close()

	if _, err := querier.Read(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNegotiateStopsProbingAfterARejectedLogin(t *testing.T) {
	// A device that has refused the login will refuse it over every other
	// transport too, so the remaining candidates are not tried.
	_, iotPort := newUDPServer(t, func([]byte) []byte { return nil })
	_, smartPort := newUDPServer(t, func([]byte) []byte { return nil })
	withDiscoveryPorts(t, iotPort, smartPort)

	device := &klapDevice{t: t, credentials: Credentials{Username: "owner@example.com", Password: "correct"},
		loginVersion: klapLoginV2, respond: func([]byte) []byte { return nil }}
	srv := httptest.NewServer(device)
	defer srv.Close()
	_, klapPort := splitHostPort(t, strings.TrimPrefix(srv.URL, "http://"))

	prevOrder := probeOrder
	probeOrder = func() []*discoveryResult {
		candidate := &discoveryResult{EncryptType: encryptKLAP, Family: "SMART", LoginVersion: klapLoginV2, HTTPPort: klapPort}
		// Three chances to keep going, so stopping after the first is a
		// decision rather than simply running out of candidates.
		return []*discoveryResult{candidate, candidate, candidate}
	}
	t.Cleanup(func() { probeOrder = prevOrder })

	_, err := negotiate(t.Context(), "127.0.0.1", Credentials{Username: "wrong@example.com", Password: "wrong"}, time.Second, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	// One handshake, not three: the first rejection ended the probing.
	if got := device.handshakeCount(); got != 1 {
		t.Fatalf("want the probing stopped after the first rejection, got %d handshakes", got)
	}
}

func TestNegotiateReportsAFailedCandidate(t *testing.T) {
	// A candidate that cannot even be built is recorded and the next one tried.
	_, iotPort := newUDPServer(t, func([]byte) []byte { return nil })
	_, smartPort := newUDPServer(t, func([]byte) []byte { return nil })
	withDiscoveryPorts(t, iotPort, smartPort)

	prevOrder := probeOrder
	probeOrder = func() []*discoveryResult {
		return []*discoveryResult{{EncryptType: "ROT13"}}
	}
	t.Cleanup(func() { probeOrder = prevOrder })

	_, err := negotiate(t.Context(), "127.0.0.1", Credentials{}, 500*time.Millisecond, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ROT13") {
		t.Fatalf("want the failed candidate named, got %q", err)
	}
}

func TestNegotiateStopsOnARejectedLogin(t *testing.T) {
	// A device that has already refused the login will refuse it over every
	// other transport too, so the remaining attempts are pointless.
	_, baseURL := newKLAPServer(t, &klapDevice{
		credentials: Credentials{Username: "owner@example.com", Password: "correct"},
		respond:     func([]byte) []byte { return nil },
	})
	host, port := splitHostPort(t, strings.TrimPrefix(strings.TrimSuffix(baseURL, "/app"), "http://"))

	querier, err := connectAs(host, Credentials{Username: "someone@example.com", Password: "wrong"}, time.Second,
		&discoveryResult{EncryptType: encryptKLAP, Family: "SMART", HTTPPort: port, LoginVersion: klapLoginV2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = querier.Read(t.Context())
	if !errors.Is(err, errKlapUnauthorized) {
		t.Fatalf("want an unauthorized error, got %v", err)
	}
}

func TestHTTPClientRefusesRedirects(t *testing.T) {
	// These are LAN devices answering on a fixed path. A redirect is either a
	// captive portal or something pretending to be a plug, and following it
	// would send the session cookie somewhere it does not belong.
	client := httpClient(time.Second)
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("want redirects refused, got %v", err)
	}
}

func TestDescribeNamesEveryTransport(t *testing.T) {
	tests := []struct {
		found *discoveryResult
		want  string
	}{
		{found: &discoveryResult{EncryptType: encryptXOR}, want: "9999"},
		{found: &discoveryResult{EncryptType: encryptAES}, want: "AES"},
		{found: &discoveryResult{EncryptType: encryptKLAP, Family: "SMART"}, want: "KLAP"},
		{found: &discoveryResult{EncryptType: "ROT13"}, want: "ROT13"},
	}

	for _, tt := range tests {
		if got := tt.found.describe(); !strings.Contains(got, tt.want) {
			t.Errorf("want %q to mention %q", got, tt.want)
		}
	}
}

func TestParseSMARTDiscoveryWithoutAScheme(t *testing.T) {
	// Firmware that answers without naming a transport is assumed to speak
	// KLAP, which is what everything that answers on this port speaks.
	reply := append(bytes.Repeat([]byte{0}, discoveryHeaderSMART),
		[]byte(`{"error_code":0,"result":{"device_type":"SMART.KASAPLUG","device_model":"EP25(US)"}}`)...)

	got := parseDiscoveryReply("192.168.1.50", discoveryPortSMART, reply)
	if got == nil {
		t.Fatal("want a discovery result, got none")
	}
	if want, have := encryptKLAP, got.EncryptType; want != have {
		t.Fatalf("want encryption %q, have %q", want, have)
	}
}

// readAll reads a request body, returning it and any error.
func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	buffer := &bytes.Buffer{}
	_, err := buffer.ReadFrom(r.Body)
	return buffer.Bytes(), err
}
