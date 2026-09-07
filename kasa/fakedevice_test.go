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
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// The fakes in this file implement the device half of each protocol, so the
// transports can be exercised end to end rather than against a recorded
// response. A handshake that derives its keys differently from the firmware
// fails here in exactly the way it would fail against a real plug.

// klapDevice is a device that speaks KLAP. It performs the real handshake and
// really encrypts, so a mistake anywhere in the derivation shows up as a failed
// exchange rather than as a passing test.
type klapDevice struct {
	t            *testing.T
	credentials  Credentials
	loginVersion int
	// respond returns the reply to one decrypted request.
	respond func(request []byte) []byte

	mu         sync.Mutex
	localSeed  []byte
	remoteSeed []byte
	authHash   []byte
	// expireAfter makes the device forget its session after this many requests,
	// so the transport's re-handshake path can be exercised. Zero never expires.
	expireAfter int
	requests    int
	handshakes  int
}

// newKLAPServer starts a fake KLAP device and returns it with its base URL.
func newKLAPServer(t *testing.T, device *klapDevice) (*klapDevice, string) {
	t.Helper()
	device.t = t
	if device.loginVersion == 0 {
		device.loginVersion = klapLoginV2
	}

	srv := httptest.NewServer(device)
	t.Cleanup(srv.Close)
	return device, srv.URL + "/app"
}

func (d *klapDevice) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch r.URL.Path {
	case "/app/handshake1":
		d.handshake1(w, body)
	case "/app/handshake2":
		d.handshake2(w, body)
	case "/app/request":
		d.request(w, r, body)
	default:
		http.NotFound(w, r)
	}
}

func (d *klapDevice) handshake1(w http.ResponseWriter, body []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(body) != klapSeedLen {
		http.Error(w, "bad seed", http.StatusBadRequest)
		return
	}
	d.handshakes++
	d.localSeed = append([]byte(nil), body...)
	d.remoteSeed = make([]byte, klapSeedLen)
	if _, err := rand.Read(d.remoteSeed); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d.authHash = klapAuthHash(d.credentials, d.loginVersion)

	http.SetCookie(w, &http.Cookie{Name: "TP_SESSIONID", Value: "session-" + strconv.Itoa(d.handshakes)})
	_, _ = w.Write(append(append([]byte(nil), d.remoteSeed...), klapSHA256(d.localSeed, d.remoteSeed, d.authHash)...))
}

func (d *klapDevice) handshake2(w http.ResponseWriter, body []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !bytes.Equal(body, klapSHA256(d.remoteSeed, d.localSeed, d.authHash)) {
		http.Error(w, "bad handshake", http.StatusForbidden)
		return
	}
	d.requests = 0
	w.WriteHeader(http.StatusOK)
}

func (d *klapDevice) request(w http.ResponseWriter, r *http.Request, body []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.authHash == nil {
		http.Error(w, "no session", http.StatusForbidden)
		return
	}
	d.requests++
	if d.expireAfter > 0 && d.requests > d.expireAfter {
		// Forget the session the way a rebooted device would.
		d.authHash = nil
		http.Error(w, "session expired", http.StatusForbidden)
		return
	}

	seq, err := strconv.Atoi(r.URL.Query().Get("seq"))
	if err != nil {
		http.Error(w, "bad seq", http.StatusBadRequest)
		return
	}

	key, sig, iv, _ := klapDeriveSession(d.localSeed, d.remoteSeed, d.authHash)
	if len(body) <= klapSigLen {
		http.Error(w, "short body", http.StatusBadRequest)
		return
	}
	ciphertext := body[klapSigLen:]

	var seqBytes [4]byte
	binary.BigEndian.PutUint32(seqBytes[:], uint32(int32(seq)))
	if !bytes.Equal(body[:klapSigLen], klapSHA256(sig, seqBytes[:], ciphertext)) {
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	blockIV := append(append([]byte(nil), iv...), seqBytes[:]...)

	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, blockIV).CryptBlocks(plain, ciphertext)
	plain, err = pkcs7Unpad(plain, block.BlockSize())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	reply := pkcs7Pad(d.respond(plain), block.BlockSize())
	out := make([]byte, len(reply))
	cipher.NewCBCEncrypter(block, blockIV).CryptBlocks(out, reply)
	_, _ = w.Write(append(klapSHA256(sig, seqBytes[:], out), out...))
}

// handshakeCount reports how many handshakes the device has performed, which is
// what tells a test whether a session was reused or rebuilt.
func (d *klapDevice) handshakeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.handshakes
}

// xorDevice is a device that speaks the legacy protocol on a TCP port.
type xorDevice struct {
	listener net.Listener
	respond  func(request []byte) []byte

	mu      sync.Mutex
	queries int
}

// newXORServer starts a fake legacy device and returns it with the address it
// listens on.
func newXORServer(t *testing.T, respond func(request []byte) []byte) (*xorDevice, string) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	device := &xorDevice{listener: listener, respond: respond}
	t.Cleanup(func() { _ = listener.Close() })

	go device.serve()
	return device, listener.Addr().String()
}

func (d *xorDevice) serve() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		go d.handle(conn)
	}
}

func (d *xorDevice) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return
	}
	body := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := io.ReadFull(conn, body); err != nil {
		return
	}

	d.mu.Lock()
	d.queries++
	d.mu.Unlock()

	reply := xorEncrypt(d.respond(xorDecrypt(body)))
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], uint32(len(reply)))
	_, _ = conn.Write(append(out[:], reply...))
}

// queryCount reports how many requests the device has answered.
func (d *xorDevice) queryCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.queries
}

// udpDevice answers discovery probes on a loopback port, so the send/receive
// cycle is exercised for real rather than by handing bytes straight to the
// parser.
type udpDevice struct {
	conn  *net.UDPConn
	reply func(request []byte) []byte

	mu       sync.Mutex
	requests int
}

// newUDPServer starts a fake device answering discovery on a loopback port.
func newUDPServer(t *testing.T, reply func(request []byte) []byte) (*udpDevice, int) {
	t.Helper()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	device := &udpDevice{conn: conn, reply: reply}
	t.Cleanup(func() { _ = conn.Close() })

	go device.serve()
	return device, conn.LocalAddr().(*net.UDPAddr).Port
}

func (d *udpDevice) serve() {
	buffer := make([]byte, 4096)
	for {
		n, addr, err := d.conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}

		d.mu.Lock()
		d.requests++
		d.mu.Unlock()

		if reply := d.reply(buffer[:n]); reply != nil {
			_, _ = d.conn.WriteToUDP(reply, addr)
		}
	}
}

// requestCount reports how many probes the device has received, which is what
// tells a test whether the repeat packets were actually sent.
func (d *udpDevice) requestCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.requests
}

// splitHostPort returns the host and port of a "host:port" address, failing the
// test if it cannot be parsed.
func splitHostPort(t *testing.T, address string) (string, int) {
	t.Helper()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("splitting %q: %v", address, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parsing port %q: %v", port, err)
	}
	return host, number
}

// jsonBytes marshals v, failing the test on error.
func jsonBytes(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return data
}
