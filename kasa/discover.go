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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Devices answer discovery on two UDP ports. The legacy IOT range replies on
// 9999 to an XOR-obfuscated sysinfo request; the SMART range replies on 20002
// to a fixed probe, and its answer is what says which transport and login
// version the device wants.
//
// They are vars only so tests can point the probes at a fake device on a
// loopback port; the firmware listens on these two and nothing else.
var (
	discoveryPortIOT   = 9999
	discoveryPortSMART = 20002
)

// discoveryProbeSMART is the fixed 16-byte packet the SMART range answers to.
// It is a constant of the protocol rather than a message with fields.
var discoveryProbeSMART, _ = hex.DecodeString("020000010000000000000000463cb5d3")

// discoveryHeaderSMART is the length of the binary header that precedes the
// JSON in a port-20002 reply.
const discoveryHeaderSMART = 16

// discoveryMaxDatagram bounds a single reply. A sysinfo datagram from an HS300
// with six outlets is around 2 KiB, so this leaves generous headroom while
// keeping the read buffer small enough to allocate once per discovery.
const discoveryMaxDatagram = 64 << 10

// Encryption schemes a device can advertise. XOR is not advertised by anything:
// it is what a reply on port 9999 implies.
const (
	encryptXOR  = "XOR"
	encryptKLAP = "KLAP"
	encryptAES  = "AES"
)

// discoveryResult is what a device said about itself when it answered
// discovery. It is enough to build a connection without probing: the transport,
// the port and the login version all come from here.
type discoveryResult struct {
	Host         string
	Alias        string
	Model        string
	DeviceID     string
	MAC          string
	Family       string
	EncryptType  string
	HTTPPort     int
	LoginVersion int
	HTTPS        bool
}

// dialect reports which message dialect a device with this family speaks.
func (d *discoveryResult) dialect() Protocol {
	if strings.HasPrefix(strings.ToUpper(d.Family), "SMART") {
		return ProtocolSMART
	}
	return ProtocolIOT
}

// loginVersion returns the KLAP login version to authenticate with.
//
// The device usually says, in the "lv" field of its discovery reply, and that
// answer is taken as given. When it does not, the family decides: the SMART
// range is version 2 throughout, while the IOT range is version 1 unless its
// firmware says otherwise — which is exactly the case that makes a correct
// password look wrong on an HS300 running hardware 2.0 firmware.
func (d *discoveryResult) loginVersion() int {
	if d.LoginVersion != 0 {
		return d.LoginVersion
	}
	if d.dialect() == ProtocolSMART {
		return klapLoginV2
	}
	return klapLoginV1
}

// port returns the HTTP port the device serves its transport on.
func (d *discoveryResult) port() int {
	if d.HTTPPort != 0 {
		return d.HTTPPort
	}
	return 80
}

// smartDiscoveryReply is a port-20002 answer.
type smartDiscoveryReply struct {
	ErrorCode int `json:"error_code"`
	Result    struct {
		DeviceID      string `json:"device_id"`
		DeviceType    string `json:"device_type"`
		DeviceModel   string `json:"device_model"`
		Alias         string `json:"alias"`
		IP            string `json:"ip"`
		MAC           string `json:"mac"`
		EncryptScheme *struct {
			IsSupportHTTPS bool   `json:"is_support_https"`
			EncryptType    string `json:"encrypt_type"`
			HTTPPort       int    `json:"http_port"`
			LoginVersion   int    `json:"lv"`
		} `json:"mgt_encrypt_schm"`
	} `json:"result"`
}

// discoverer sends the two probes and collects whatever answers arrive before
// the deadline.
type discoverer struct {
	conn    *net.UDPConn
	packets int
}

// newDiscoverer opens a UDP socket able to send broadcasts.
func newDiscoverer(ctx context.Context, packets int) (*discoverer, error) {
	config := net.ListenConfig{Control: enableBroadcast}
	conn, err := config.ListenPacket(ctx, "udp4", ":0")
	if err != nil {
		return nil, err
	}
	udp, ok := conn.(*net.UDPConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("discovery socket is not a UDP connection")
	}
	return &discoverer{conn: udp, packets: packets}, nil
}

// probe sends both discovery packets to one address.
//
// UDP to a broadcast address is lossy in a way a unicast request is not — a
// busy device simply misses the datagram — so the probe is repeated. That is
// why a device sometimes needs a second discovery pass to appear, and why
// sending more than one packet is cheaper than waiting a whole interval for the
// next pass.
func (d *discoverer) probe(target string) error {
	iotProbe := xorEncrypt([]byte(`{"system":{"get_sysinfo":{}}}`))

	iotAddr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(target, fmt.Sprint(discoveryPortIOT)))
	if err != nil {
		return err
	}
	smartAddr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(target, fmt.Sprint(discoveryPortSMART)))
	if err != nil {
		return err
	}

	var sendErr error
	for i := 0; i < d.packets; i++ {
		if _, err := d.conn.WriteToUDP(iotProbe, iotAddr); err != nil {
			sendErr = err
		}
		if _, err := d.conn.WriteToUDP(discoveryProbeSMART, smartAddr); err != nil {
			sendErr = err
		}
	}
	// Only a failure to send *both* probes is fatal: a network where broadcast
	// is filtered on one port but not the other should still discover what it
	// can.
	return sendErr
}

// collect reads replies until the deadline passes, returning one result per
// device address. Later replies for an address win, so a device that answers on
// both ports is recorded with the more specific SMART details.
func (d *discoverer) collect(deadline time.Time) map[string]*discoveryResult {
	found := make(map[string]*discoveryResult)
	buffer := make([]byte, discoveryMaxDatagram)

	for {
		if err := d.conn.SetReadDeadline(deadline); err != nil {
			return found
		}
		n, addr, err := d.conn.ReadFromUDP(buffer)
		if err != nil {
			// A timeout is the normal way this loop ends; anything else is a
			// broken socket, and either way there is nothing more to read.
			return found
		}

		host := addr.IP.String()
		if result := parseDiscoveryReply(host, addr.Port, buffer[:n]); result != nil {
			// A device that answers on both ports is recorded from the SMART
			// reply, which is the only one that names the transport.
			if existing, ok := found[host]; ok && existing.EncryptType != encryptXOR && result.EncryptType == encryptXOR {
				continue
			}
			found[host] = result
		}
	}
}

// Close releases the discovery socket.
func (d *discoverer) Close() { _ = d.conn.Close() }

// parseDiscoveryReply decodes one datagram, returning nil for anything that is
// not a recognisable device reply.
func parseDiscoveryReply(host string, port int, data []byte) *discoveryResult {
	switch port {
	case discoveryPortIOT:
		return parseIOTDiscovery(host, data)
	case discoveryPortSMART:
		return parseSMARTDiscovery(host, data)
	}
	return nil
}

// parseIOTDiscovery decodes a port-9999 reply, which is a complete sysinfo
// under the XOR obfuscation.
func parseIOTDiscovery(host string, data []byte) *discoveryResult {
	var response iotResponse
	if err := json.Unmarshal(xorDecrypt(data), &response); err != nil {
		return nil
	}
	if response.System == nil || response.System.GetSysinfo == nil {
		return nil
	}
	sysinfo := response.System.GetSysinfo

	return &discoveryResult{
		Host:  host,
		Alias: sysinfo.Alias,
		Model: sysinfo.Model,
		// A device that answers on 9999 speaks the legacy transport by
		// definition: the port is only open on firmware that still supports it.
		DeviceID:    sysinfo.DeviceID,
		MAC:         sysinfo.mac(),
		Family:      sysinfo.family(),
		EncryptType: encryptXOR,
	}
}

// parseSMARTDiscovery decodes a port-20002 reply: a fixed binary header
// followed by the JSON that names the device's transport.
func parseSMARTDiscovery(host string, data []byte) *discoveryResult {
	if len(data) <= discoveryHeaderSMART {
		return nil
	}

	var reply smartDiscoveryReply
	if err := json.Unmarshal(data[discoveryHeaderSMART:], &reply); err != nil {
		return nil
	}
	if reply.ErrorCode != 0 || reply.Result.DeviceModel == "" {
		return nil
	}

	result := &discoveryResult{
		Host:     host,
		Alias:    reply.Result.Alias,
		Model:    reply.Result.DeviceModel,
		DeviceID: reply.Result.DeviceID,
		MAC:      reply.Result.MAC,
		Family:   reply.Result.DeviceType,
	}
	if scheme := reply.Result.EncryptScheme; scheme != nil {
		result.EncryptType = strings.ToUpper(scheme.EncryptType)
		result.HTTPPort = scheme.HTTPPort
		result.LoginVersion = scheme.LoginVersion
		result.HTTPS = scheme.IsSupportHTTPS
	}
	if result.EncryptType == "" {
		result.EncryptType = encryptKLAP
	}
	return result
}

// discover broadcasts to target and returns every device that answered.
func discover(ctx context.Context, target string, timeout time.Duration, packets int) ([]*discoveryResult, error) {
	d, err := newDiscoverer(ctx, packets)
	if err != nil {
		return nil, err
	}
	defer d.Close()

	if err := d.probe(target); err != nil {
		return nil, fmt.Errorf("sending discovery probes to %s: %w", target, err)
	}

	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	found := d.collect(deadline)
	results := make([]*discoveryResult, 0, len(found))
	for _, result := range found {
		results = append(results, result)
	}
	return results, nil
}

// discoverHost asks a single device to describe itself, so an explicitly
// configured host is connected the same informed way a discovered one is
// instead of being probed transport by transport.
func discoverHost(ctx context.Context, host string, timeout time.Duration) (*discoveryResult, error) {
	results, err := discover(ctx, host, timeout, 1)
	if err != nil {
		return nil, err
	}
	// The reply's source address is the device's own, which for a host given as
	// a hostname will not match what was asked for. With a single target there
	// is only ever one device to choose from, so the first answer is it.
	if len(results) == 0 {
		return nil, fmt.Errorf("no answer from %s", host)
	}
	result := results[0]
	result.Host = host
	return result, nil
}
