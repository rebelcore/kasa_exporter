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
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- protocol-mandated: the login hashes the username with SHA-1.
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// The AES transport — TP-Link calls it "secure passthrough" — is what SMART
// devices spoke before KLAP: an RSA key exchange establishes an AES-128-CBC
// session key, a login exchanges credentials for a token, and every later
// request travels as base64 ciphertext inside a JSON envelope.
//
// Newer firmware, including the EP25, has moved to KLAP; this path is what
// keeps older Tapo-generation hardware on the same exporter.
const (
	// aesRSABits is fixed by the firmware: it will not accept a larger modulus.
	aesRSABits = 1024
	// aesSessionLen is the length of the RSA-wrapped secret: 16 bytes of key
	// followed by 16 bytes of IV.
	aesSessionLen = 32
)

// errAESUnauthorized reports that the device refused the login. Like its KLAP
// counterpart it stops the connect path retrying a fault that will not clear.
var errAESUnauthorized = errors.New("device rejected the supplied credentials")

// aesEnvelope is the outer JSON of every AES-transport exchange. Params is left
// as a raw message on the way in so handshake and passthrough responses can
// share one decoder.
type aesEnvelope struct {
	ErrorCode int             `json:"error_code"`
	Result    json.RawMessage `json:"result"`
}

// aesTransport holds one authenticated secure-passthrough session.
type aesTransport struct {
	client      *http.Client
	baseURL     string
	credentials Credentials

	mu          sync.Mutex
	key         []byte
	iv          []byte
	token       string
	sessionID   string
	established bool
}

// Query sends one JSON request through the session and returns the JSON
// response. As with KLAP, an expired session is re-established once: devices
// drop sessions on their own schedule, and a scrape must not fail for the rest
// of the process's life because of it.
func (t *aesTransport) Query(ctx context.Context, request []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for attempt := 0; attempt < 2; attempt++ {
		if !t.established {
			if err := t.connect(ctx); err != nil {
				return nil, err
			}
		}

		plain, err := t.passthrough(ctx, request)
		if err != nil {
			if errors.Is(err, errAESSessionExpired) {
				t.established = false
				continue
			}
			return nil, err
		}
		return plain, nil
	}

	return nil, errors.New("session expired twice; the device may have been re-provisioned")
}

// Close forgets the session so the next query re-establishes one.
func (t *aesTransport) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.established = false
	t.token = ""
	t.sessionID = ""
}

// errAESSessionExpired marks the device's "token expired" error code, which is
// recoverable by handshaking again rather than a real failure.
var errAESSessionExpired = errors.New("session expired")

// connect performs the RSA handshake and the login, leaving the transport with
// a session key and a request token. The caller must hold t.mu.
func (t *aesTransport) connect(ctx context.Context) error {
	t.token = ""
	t.sessionID = ""

	key, err := rsa.GenerateKey(rand.Reader, aesRSABits)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	// The firmware wants a PEM document, base64-encoded again inside the JSON.
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	handshake, err := json.Marshal(map[string]any{
		"method": "handshake",
		"params": map[string]any{"key": string(publicPEM)},
	})
	if err != nil {
		return err
	}

	result, err := t.post(ctx, "", handshake)
	if err != nil {
		return err
	}

	var wrapped struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(result, &wrapped); err != nil {
		return fmt.Errorf("unexpected handshake response: %w", err)
	}
	encrypted, err := base64.StdEncoding.DecodeString(wrapped.Key)
	if err != nil {
		return fmt.Errorf("handshake key is not valid base64: %w", err)
	}

	// PKCS #1 v1.5 is what the firmware encrypts with, so OAEP is not an option
	// here: the padding is the device's choice, not ours. The exposure the
	// deprecation warns about — an attacker learning whether each decryption
	// errored — needs a repeatable oracle against a long-lived private key, and
	// this key is generated fresh for each handshake and used exactly once. A
	// device that offers KLAP is preferred over this path anyway, which is why
	// the discovery reply's scheme decides which transport is built.
	//
	//nolint:staticcheck // SA1019: the device's protocol mandates PKCS #1 v1.5.
	secret, err := rsa.DecryptPKCS1v15(rand.Reader, key, encrypted)
	if err != nil {
		return fmt.Errorf("decrypting the session key: %w", err)
	}
	if len(secret) != aesSessionLen {
		return fmt.Errorf("session key is %d bytes, want %d", len(secret), aesSessionLen)
	}
	t.key, t.iv = secret[:16], secret[16:]
	t.established = true

	return t.login(ctx)
}

// login exchanges credentials for the request token. The caller must hold t.mu.
//
// Two login shapes exist and the device does not announce which it wants, so
// both are tried: the older one sends the password verbatim, the newer hashes
// it. Only a rejection of both is a real credential failure.
func (t *aesTransport) login(ctx context.Context) error {
	user := sha1.Sum([]byte(t.credentials.Username)) // #nosec G401 -- protocol-mandated digest.
	username := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(user[:])))
	pass := sha1.Sum([]byte(t.credentials.Password)) // #nosec G401 -- protocol-mandated digest.

	attempts := []map[string]any{
		{"username": username, "password": base64.StdEncoding.EncodeToString([]byte(t.credentials.Password))},
		{"username": username, "password2": base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(pass[:])))},
	}

	var lastErr error
	for _, params := range attempts {
		request, err := json.Marshal(map[string]any{
			"method":              "login_device",
			"params":              params,
			"requestTimeMils":     time.Now().UnixMilli(),
			"request_time_milis":  time.Now().UnixMilli(),
			"terminal_uuid":       terminalUUID,
			"request_id_reserved": nil,
		})
		if err != nil {
			return err
		}

		plain, err := t.passthrough(ctx, request)
		if err != nil {
			lastErr = err
			continue
		}

		var envelope struct {
			Result struct {
				Token string `json:"token"`
			} `json:"result"`
		}
		if err := json.Unmarshal(plain, &envelope); err != nil {
			lastErr = fmt.Errorf("unexpected login response: %w", err)
			continue
		}
		if envelope.Result.Token == "" {
			lastErr = errors.New("login returned no token")
			continue
		}
		t.token = envelope.Result.Token
		return nil
	}

	t.established = false
	if lastErr != nil {
		return fmt.Errorf("%w: %s", errAESUnauthorized, lastErr)
	}
	return errAESUnauthorized
}

// passthrough encrypts one request, posts it inside the securePassthrough
// envelope, and returns the decrypted reply verbatim. The caller must hold
// t.mu.
//
// The reply keeps its {"error_code":..,"result":..} envelope so that both
// transports hand the dialect above them exactly what the device sent; only the
// two error codes this layer acts on — an expired session and a rejected login
// — are interpreted here.
func (t *aesTransport) passthrough(ctx context.Context, request []byte) ([]byte, error) {
	ciphertext, err := t.encrypt(request)
	if err != nil {
		return nil, err
	}

	envelope, err := json.Marshal(map[string]any{
		"method": "securePassthrough",
		"params": map[string]any{"request": base64.StdEncoding.EncodeToString(ciphertext)},
	})
	if err != nil {
		return nil, err
	}

	query := ""
	if t.token != "" {
		query = "?token=" + t.token
	}
	result, err := t.post(ctx, query, envelope)
	if err != nil {
		return nil, err
	}

	var wrapped struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(result, &wrapped); err != nil {
		return nil, fmt.Errorf("unexpected passthrough response: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(wrapped.Response)
	if err != nil {
		return nil, fmt.Errorf("passthrough response is not valid base64: %w", err)
	}

	plain, err := t.decrypt(raw)
	if err != nil {
		return nil, err
	}

	// The inner envelope carries its own error code, so a device that refuses
	// the request answers HTTP 200 with a failure inside the ciphertext.
	var inner aesEnvelope
	if err := json.Unmarshal(plain, &inner); err != nil {
		return nil, fmt.Errorf("unexpected inner response: %w", err)
	}
	if err := aesErrorFor(inner.ErrorCode); err != nil {
		return nil, err
	}
	return plain, nil
}

// post sends one plain JSON envelope to the device and returns its result
// member, tracking the session cookie the device sets during the handshake.
func (t *aesTransport) post(ctx context.Context, query string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+query, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent())
	if t.sessionID != "" {
		req.AddCookie(&http.Cookie{Name: "TP_SESSIONID", Value: t.sessionID})
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxResponseBytes {
		return nil, fmt.Errorf("response from %s exceeds %d byte limit", t.baseURL, maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request returned HTTP %d", resp.StatusCode)
	}

	for _, c := range resp.Cookies() {
		if c.Name == "TP_SESSIONID" {
			t.sessionID = c.Value
		}
	}

	var envelope aesEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("unexpected response: %w", err)
	}
	if err := aesErrorFor(envelope.ErrorCode); err != nil {
		return nil, err
	}
	return envelope.Result, nil
}

// encrypt AES-128-CBC encrypts a request under the session key.
func (t *aesTransport) encrypt(payload []byte) ([]byte, error) {
	block, err := aes.NewCipher(t.key)
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(payload, block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, t.iv).CryptBlocks(out, padded)
	return out, nil
}

// decrypt reverses encrypt.
func (t *aesTransport) decrypt(payload []byte) ([]byte, error) {
	block, err := aes.NewCipher(t.key)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload)%block.BlockSize() != 0 {
		return nil, fmt.Errorf("ciphertext of %d bytes is not a positive multiple of the block size", len(payload))
	}
	out := make([]byte, len(payload))
	cipher.NewCBCDecrypter(block, t.iv).CryptBlocks(out, payload)
	return pkcs7Unpad(out, block.BlockSize())
}

// aesErrorFor maps a firmware error code onto an error, distinguishing the two
// that the caller acts on — an expired session, which is retried, and a
// rejected login, which is not.
func aesErrorFor(code int) error {
	switch code {
	case 0:
		return nil
	case -1501, -1002:
		return fmt.Errorf("%w (error code %d)", errAESUnauthorized, code)
	case 9999, -1301:
		return errAESSessionExpired
	default:
		return fmt.Errorf("device returned error code %d", code)
	}
}
