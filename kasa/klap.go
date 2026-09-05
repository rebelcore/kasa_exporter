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
	"crypto/md5" // #nosec G501 -- protocol-mandated: KLAP v1 derives its auth hash with MD5.
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- protocol-mandated: KLAP v2 derives its auth hash with SHA-1.
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// KLAP is TP-Link's authenticated session protocol, spoken over HTTP by both
// newer IOT firmware (an HS300 on hardware 2.0) and the whole SMART range (an
// EP25). A session is established with two unencrypted handshake posts and then
// every request is AES-128-CBC encrypted and signed.
//
// Two login versions exist and differ only in how the credentials are hashed
// into the shared secret. The device advertises which one it wants in the "lv"
// field of its discovery response; when that is missing, both are tried.
const (
	klapLoginV1 = 1
	klapLoginV2 = 2
)

// Handshake and framing sizes, all fixed by the protocol.
const (
	klapSeedLen      = 16
	klapHashLen      = 32
	klapHandshakeLen = klapSeedLen + klapHashLen
	klapSigLen       = 32
)

// Default logins the firmware accepts when a device was never bound to a
// TP-Link account. Tried after the operator's own credentials so a configured
// login always wins.
var klapFallbackCredentials = []Credentials{
	{}, // A factory-reset device authenticates with empty strings.
	{Username: "kasa@tp-link.net", Password: "kasaSetup"},
	{Username: "test@tp-link.net", Password: "test"},
}

// errKlapUnauthorized reports that no candidate credential produced the hash
// the device expected. It is returned rather than a generic error so the
// connect path can stop retrying: a rejected login is a standing fault that
// another attempt will not fix.
var errKlapUnauthorized = errors.New("device rejected the supplied credentials")

// Credentials is a TP-Link cloud login. The zero value means "no credentials",
// which is what a device that was never bound to an account expects.
type Credentials struct {
	Username string
	Password string
}

// klapTransport holds one authenticated session with a device. Sessions are
// reused across scrapes; the mutex serialises access because the sequence
// number must advance exactly once per request.
type klapTransport struct {
	client       *http.Client
	baseURL      string
	credentials  Credentials
	loginVersion int

	mu        sync.Mutex
	sessionID string
	encKey    []byte
	sigKey    []byte
	iv        []byte
	seq       int32
	// established is false before the first handshake and after a session is
	// invalidated, which is what makes Query re-handshake on the next call.
	established bool
}

// klapAuthHash derives the shared secret from a login, using whichever digest
// the device's login version calls for.
//
// Login version 2 exists because TP-Link moved off MD5, and python-kasa maps
// the whole IOT.KLAP family onto version 1 regardless of what the device
// advertised. Firmware reporting "lv": 2 — the build found on HS300 hardware
// 2.0 among others — then fails the handshake with what looks like a wrong
// password and is not one, which is why the version is tracked per device here.
func klapAuthHash(creds Credentials, loginVersion int) []byte {
	if loginVersion == klapLoginV2 {
		user := sha1.Sum([]byte(creds.Username)) // #nosec G401 -- protocol-mandated digest.
		pass := sha1.Sum([]byte(creds.Password)) // #nosec G401 -- protocol-mandated digest.
		sum := sha256.Sum256(append(user[:], pass[:]...))
		return sum[:]
	}
	user := md5.Sum([]byte(creds.Username)) // #nosec G401 -- protocol-mandated digest.
	pass := md5.Sum([]byte(creds.Password)) // #nosec G401 -- protocol-mandated digest.
	sum := md5.Sum(append(user[:], pass[:]...))
	return sum[:]
}

// klapSHA256 concatenates its arguments and returns their SHA-256 digest. Every
// KLAP derivation is a hash over an ordered set of byte strings, so writing it
// once keeps those derivations readable.
func klapSHA256(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// klapDeriveSession turns the exchanged seeds and the authenticated hash into
// the session's encryption key, signing key, IV prefix and starting sequence
// number. The "lsk", "ldk" and "iv" prefixes are the protocol's own domain
// separators.
func klapDeriveSession(localSeed, remoteSeed, authHash []byte) (encKey, sigKey, iv []byte, seq int32) {
	encKey = klapSHA256([]byte("lsk"), localSeed, remoteSeed, authHash)[:16]
	sigKey = klapSHA256([]byte("ldk"), localSeed, remoteSeed, authHash)[:28]
	fullIV := klapSHA256([]byte("iv"), localSeed, remoteSeed, authHash)
	// The IV is only 12 bytes: the remaining four are the sequence number,
	// appended per request so every message encrypts under a distinct IV.
	iv = fullIV[:12]
	seq = int32(binary.BigEndian.Uint32(fullIV[28:32]))
	return encKey, sigKey, iv, seq
}

// klapPost sends one raw HTTP POST to a KLAP endpoint and returns the status,
// body and any TP_SESSIONID cookie the device set.
func (t *klapTransport) klapPost(ctx context.Context, path string, body []byte) (int, []byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, "", err
	}
	req.Header.Set("User-Agent", userAgent())
	if t.sessionID != "" {
		req.AddCookie(&http.Cookie{Name: "TP_SESSIONID", Value: t.sessionID})
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded like every other read from a device: the body length is under the
	// device's control, and a KLAP response never legitimately approaches this.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return resp.StatusCode, nil, "", err
	}
	if int64(len(data)) > maxResponseBytes {
		return resp.StatusCode, nil, "", fmt.Errorf("response from %s exceeds %d byte limit", t.baseURL+path, maxResponseBytes)
	}

	session := ""
	for _, c := range resp.Cookies() {
		if c.Name == "TP_SESSIONID" {
			session = c.Value
		}
	}
	return resp.StatusCode, data, session, nil
}

// handshake performs the two-post KLAP exchange and installs the resulting
// session. The caller must hold t.mu.
//
// Handshake 1 is what proves the credentials: the device returns a hash over
// both seeds and its own copy of the login, so a candidate login can be checked
// locally before anything is sent back. That is why several candidates can be
// tried from a single round trip rather than one login per attempt.
func (t *klapTransport) handshake(ctx context.Context) error {
	localSeed := make([]byte, klapSeedLen)
	if _, err := rand.Read(localSeed); err != nil {
		return err
	}

	// A stale cookie must not be sent with a new handshake: the device would
	// answer for the old session and the derived keys would not match.
	t.sessionID = ""
	status, data, session, err := t.klapPost(ctx, "/handshake1", localSeed)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("handshake1 returned HTTP %d", status)
	}
	if len(data) != klapHandshakeLen {
		return fmt.Errorf("handshake1 returned %d bytes, want %d", len(data), klapHandshakeLen)
	}
	remoteSeed, serverHash := data[:klapSeedLen], data[klapSeedLen:]

	// The operator's credentials first, so a configured login is never
	// shadowed by a factory default that happens to also be accepted.
	candidates := append([]Credentials{t.credentials}, klapFallbackCredentials...)
	var authHash []byte
	for _, creds := range candidates {
		candidate := klapAuthHash(creds, t.loginVersion)
		if bytes.Equal(klapSHA256(localSeed, remoteSeed, candidate), serverHash) {
			authHash = candidate
			break
		}
	}
	if authHash == nil {
		return errKlapUnauthorized
	}

	t.sessionID = session
	status, _, _, err = t.klapPost(ctx, "/handshake2", klapSHA256(remoteSeed, localSeed, authHash))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("handshake2 returned HTTP %d", status)
	}

	t.encKey, t.sigKey, t.iv, t.seq = klapDeriveSession(localSeed, remoteSeed, authHash)
	t.established = true
	return nil
}

// klapEncrypt encrypts and signs one payload for the given sequence number,
// returning the wire body (signature followed by ciphertext).
func (t *klapTransport) klapEncrypt(payload []byte, seq int32) ([]byte, error) {
	block, err := aes.NewCipher(t.encKey)
	if err != nil {
		return nil, err
	}

	padded := pkcs7Pad(payload, block.BlockSize())
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, t.blockIV(seq)).CryptBlocks(ciphertext, padded)

	var seqBytes [4]byte
	binary.BigEndian.PutUint32(seqBytes[:], uint32(seq))
	signature := klapSHA256(t.sigKey, seqBytes[:], ciphertext)

	return append(signature, ciphertext...), nil
}

// klapDecrypt decrypts a response body, discarding the signature prefix. The
// signature is not verified: it is computed with the same session key the
// exporter already used to encrypt the request, so a device that could forge it
// is a device that already holds the session.
func (t *klapTransport) klapDecrypt(body []byte, seq int32) ([]byte, error) {
	if len(body) <= klapSigLen {
		return nil, fmt.Errorf("response is %d bytes, too short to contain a signature", len(body))
	}
	ciphertext := body[klapSigLen:]

	block, err := aes.NewCipher(t.encKey)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%block.BlockSize() != 0 {
		return nil, fmt.Errorf("ciphertext of %d bytes is not a multiple of the block size", len(ciphertext))
	}

	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, t.blockIV(seq)).CryptBlocks(plaintext, ciphertext)
	return pkcs7Unpad(plaintext, block.BlockSize())
}

// blockIV builds the per-request initialisation vector: the session's 12-byte
// IV prefix followed by the request's sequence number.
func (t *klapTransport) blockIV(seq int32) []byte {
	iv := make([]byte, 0, aes.BlockSize)
	iv = append(iv, t.iv...)
	var seqBytes [4]byte
	binary.BigEndian.PutUint32(seqBytes[:], uint32(seq))
	return append(iv, seqBytes[:]...)
}

// Query sends one JSON request over the session and returns the JSON response.
//
// A session that the device has since forgotten — after a reboot, or simply
// after its idle timeout — answers with 403 rather than an error the HTTP
// client would surface. That is retried once against a fresh handshake, because
// otherwise every scrape after a device reboots fails until the exporter is
// restarted.
func (t *klapTransport) Query(ctx context.Context, request []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for attempt := 0; attempt < 2; attempt++ {
		if !t.established {
			if err := t.handshake(ctx); err != nil {
				return nil, err
			}
		}

		t.seq++
		seq := t.seq
		body, err := t.klapEncrypt(request, seq)
		if err != nil {
			return nil, err
		}

		status, data, _, err := t.klapPost(ctx, fmt.Sprintf("/request?seq=%d", seq), body)
		if err != nil {
			return nil, err
		}
		if status == http.StatusForbidden {
			t.established = false
			continue
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("request returned HTTP %d", status)
		}
		return t.klapDecrypt(data, seq)
	}

	return nil, errors.New("session was rejected twice; the device may have been re-provisioned")
}

// Close forgets the session so the next query re-handshakes.
func (t *klapTransport) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.established = false
	t.sessionID = ""
}

// pkcs7Pad appends PKCS#7 padding, always adding at least one byte so the
// padding length is unambiguous.
func pkcs7Pad(data []byte, blockSize int) []byte {
	n := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(n)}, n)...)
}

// pkcs7Unpad removes PKCS#7 padding, rejecting a trailer that does not describe
// a valid pad rather than returning silently corrupt plaintext.
func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, fmt.Errorf("padded length %d is not a positive multiple of the block size", len(data))
	}
	n := int(data[len(data)-1])
	if n == 0 || n > blockSize || n > len(data) {
		return nil, fmt.Errorf("invalid padding length %d", n)
	}
	for _, b := range data[len(data)-n:] {
		if int(b) != n {
			return nil, errors.New("invalid padding bytes")
		}
	}
	return data[:len(data)-n], nil
}
