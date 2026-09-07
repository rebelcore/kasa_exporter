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
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// aesDevice is a device that speaks the older secure-passthrough protocol. Like
// the KLAP fake it performs the real exchange, so a mistake in the RSA
// handshake or the AES framing surfaces as a failed session.
type aesDevice struct {
	t *testing.T
	// acceptLogin decides whether the login is accepted, so the credential
	// failure path can be exercised.
	acceptLogin bool
	// loginReply overrides the answer to login_device, for firmware that
	// answers something the transport cannot use.
	loginReply []byte
	// secretLength overrides the length of the RSA-wrapped session secret, so
	// the transport's length check can be exercised. Zero means the real one.
	secretLength int
	// unwrappableKey returns a session key that was not encrypted to the
	// client's public key, as something that is not the device would.
	unwrappableKey bool
	// corruptCiphertext returns a reply the client cannot decrypt.
	corruptCiphertext bool
	// expireEveryRequest answers every request with an expired session, even
	// straight after a fresh handshake.
	expireEveryRequest bool
	respond            func(request []byte) []byte

	mu sync.Mutex
	// expireAfter makes the device answer with its "token expired" code after
	// this many requests. Zero never expires.
	expireAfter int
	requests    int
	handshakes  int
	key, iv     []byte
	token       string
}

func newAESServer(t *testing.T, device *aesDevice) (*aesDevice, string) {
	t.Helper()
	device.t = t

	srv := httptest.NewServer(device)
	t.Cleanup(srv.Close)
	return device, srv.URL + "/app"
}

func (d *aesDevice) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var request struct {
		Method string `json:"method"`
		Params struct {
			Key     string `json:"key"`
			Request string `json:"request"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch request.Method {
	case "handshake":
		d.handshake(w, request.Params.Key)
	case "securePassthrough":
		d.passthrough(w, r, request.Params.Request)
	default:
		writeJSON(w, map[string]any{"error_code": -1})
	}
}

func (d *aesDevice) handshake(w http.ResponseWriter, key string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	block, _ := pem.Decode([]byte(key))
	if block == nil {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	public, ok := parsed.(*rsa.PublicKey)
	if !ok {
		http.Error(w, "not an RSA key", http.StatusBadRequest)
		return
	}

	d.handshakes++
	d.token = ""
	// The expiry budget is per session, so a fresh handshake starts it over.
	d.requests = 0

	length := aesSessionLen
	if d.secretLength != 0 {
		length = d.secretLength
	}
	secret := make([]byte, length)
	if _, err := rand.Read(secret); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if length == aesSessionLen {
		d.key, d.iv = secret[:16], secret[16:]
	}

	if d.unwrappableKey {
		// Encrypted to a key the client does not hold, which is what an
		// impostor on the device's address would produce.
		other, err := rsa.GenerateKey(rand.Reader, aesRSABits)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		public = &other.PublicKey
	}

	// The device half of the handshake, so the padding matches what the
	// firmware sends.
	//
	//nolint:staticcheck // SA1019: the device's protocol mandates PKCS #1 v1.5.
	encrypted, err := rsa.EncryptPKCS1v15(rand.Reader, public, secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "TP_SESSIONID", Value: "aes-session"})
	writeJSON(w, map[string]any{
		"error_code": 0,
		"result":     map[string]any{"key": base64.StdEncoding.EncodeToString(encrypted)},
	})
}

func (d *aesDevice) passthrough(w http.ResponseWriter, r *http.Request, payload string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.key == nil {
		writeJSON(w, map[string]any{"error_code": 9999})
		return
	}

	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	plain, err := d.decrypt(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var inner struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(plain, &inner); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var reply []byte
	expired := false
	switch {
	case inner.Method == "login_device":
		if d.loginReply != nil {
			reply = d.loginReply
			break
		}
		if !d.acceptLogin {
			reply = []byte(`{"error_code":-1501}`)
			break
		}
		d.token = "token-1"
		reply = []byte(`{"error_code":0,"result":{"token":"token-1"}}`)
	case r.URL.Query().Get("token") != d.token:
		reply = []byte(`{"error_code":9999}`)
	case d.expireEveryRequest:
		reply = []byte(`{"error_code":9999}`)
	default:
		d.requests++
		if d.expireAfter > 0 && d.requests > d.expireAfter {
			expired = true
			reply = []byte(`{"error_code":9999}`)
			break
		}
		reply = d.respond(plain)
	}

	if d.corruptCiphertext && inner.Method != "login_device" {
		// Not a multiple of the block size, so the client cannot decrypt it.
		writeJSON(w, map[string]any{
			"error_code": 0,
			"result":     map[string]any{"response": base64.StdEncoding.EncodeToString([]byte("12345"))},
		})
		return
	}

	// Encrypted before the session is torn down: a real device answers the
	// request that expired the session under the key that request used, and
	// only then forgets it.
	encrypted, err := d.encrypt(reply)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if expired {
		d.key, d.iv, d.token = nil, nil, ""
	}
	writeJSON(w, map[string]any{
		"error_code": 0,
		"result":     map[string]any{"response": base64.StdEncoding.EncodeToString(encrypted)},
	})
}

func (d *aesDevice) encrypt(payload []byte) ([]byte, error) {
	block, err := aes.NewCipher(d.key)
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(payload, block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, d.iv).CryptBlocks(out, padded)
	return out, nil
}

func (d *aesDevice) decrypt(payload []byte) ([]byte, error) {
	block, err := aes.NewCipher(d.key)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload)%block.BlockSize() != 0 {
		return nil, errors.New("bad ciphertext length")
	}
	out := make([]byte, len(payload))
	cipher.NewCBCDecrypter(block, d.iv).CryptBlocks(out, payload)
	return pkcs7Unpad(out, block.BlockSize())
}

func (d *aesDevice) handshakeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.handshakes
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestAESTransportQuery(t *testing.T) {
	device, baseURL := newAESServer(t, &aesDevice{
		acceptLogin: true,
		respond: func(request []byte) []byte {
			if !strings.Contains(string(request), "get_device_info") {
				t.Errorf("want a get_device_info request, got %s", request)
			}
			return []byte(`{"error_code":0,"result":{"model":"P110"}}`)
		},
	})

	transport := &aesTransport{
		client:      httpClient(2 * time.Second),
		baseURL:     baseURL,
		credentials: Credentials{Username: "user@example.com", Password: "secret"},
	}

	got, err := transport.Query(t.Context(), []byte(`{"method":"get_device_info"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := `{"error_code":0,"result":{"model":"P110"}}`, string(got); want != have {
		t.Fatalf("want %q, have %q", want, have)
	}

	if _, err := transport.Query(t.Context(), []byte(`{"method":"get_device_info"}`)); err != nil {
		t.Fatalf("unexpected error on the second query: %v", err)
	}
	if got := device.handshakeCount(); got != 1 {
		t.Fatalf("want 1 handshake across two queries, got %d", got)
	}
}

func TestAESTransportReconnectsAfterExpiry(t *testing.T) {
	device, baseURL := newAESServer(t, &aesDevice{
		acceptLogin: true,
		expireAfter: 1,
		respond:     func([]byte) []byte { return []byte(`{"error_code":0,"result":{}}`) },
	})

	transport := &aesTransport{
		client:      httpClient(2 * time.Second),
		baseURL:     baseURL,
		credentials: Credentials{Username: "user@example.com", Password: "secret"},
	}

	if _, err := transport.Query(t.Context(), []byte(`{"method":"get_device_info"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := transport.Query(t.Context(), []byte(`{"method":"get_device_info"}`)); err != nil {
		t.Fatalf("unexpected error after expiry: %v", err)
	}
	if got := device.handshakeCount(); got != 2 {
		t.Fatalf("want 2 handshakes, got %d", got)
	}
}

func TestAESTransportRejectedLogin(t *testing.T) {
	_, baseURL := newAESServer(t, &aesDevice{
		acceptLogin: false,
		respond:     func([]byte) []byte { return []byte(`{"error_code":0}`) },
	})

	transport := &aesTransport{
		client:      httpClient(2 * time.Second),
		baseURL:     baseURL,
		credentials: Credentials{Username: "user@example.com", Password: "wrong"},
	}

	if _, err := transport.Query(t.Context(), []byte(`{}`)); !errors.Is(err, errAESUnauthorized) {
		t.Fatalf("want an unauthorized error, got %v", err)
	}
}

func TestAESTransportCloseForcesReconnect(t *testing.T) {
	device, baseURL := newAESServer(t, &aesDevice{
		acceptLogin: true,
		respond:     func([]byte) []byte { return []byte(`{"error_code":0,"result":{}}`) },
	})

	transport := &aesTransport{
		client:      httpClient(2 * time.Second),
		baseURL:     baseURL,
		credentials: Credentials{Username: "user@example.com", Password: "secret"},
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

func TestAESTransportBadHandshake(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "not json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("nope"))
			},
		},
		{
			name: "key is not base64",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, map[string]any{"error_code": 0, "result": map[string]any{"key": "!!!"}})
			},
		},
		{
			name: "error status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "no", http.StatusServiceUnavailable)
			},
		},
		{
			name: "device error code",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, map[string]any{"error_code": -1010})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			transport := &aesTransport{client: httpClient(2 * time.Second), baseURL: srv.URL + "/app"}
			if _, err := transport.Query(t.Context(), []byte(`{}`)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestAESErrorFor(t *testing.T) {
	if err := aesErrorFor(0); err != nil {
		t.Errorf("want no error for code 0, got %v", err)
	}
	if err := aesErrorFor(-1501); !errors.Is(err, errAESUnauthorized) {
		t.Errorf("want an unauthorized error, got %v", err)
	}
	if err := aesErrorFor(9999); !errors.Is(err, errAESSessionExpired) {
		t.Errorf("want an expired-session error, got %v", err)
	}
	if err := aesErrorFor(-1234); err == nil {
		t.Error("want an error for an unrecognised code")
	}
}
