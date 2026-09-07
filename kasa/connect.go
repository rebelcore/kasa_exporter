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
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// negotiate builds a working connection to a device.
//
// When discovery has already described the device its answer is taken as
// given: it names the transport, the port and the login version, so exactly one
// attempt is made. Only a device reached without that description — an
// explicitly configured host that never answered discovery — is probed, and
// then the order matters. The legacy transport is tried last, not first,
// because a device on newer firmware has port 9999 closed: leading with it
// would mean the attempt that always fails is also the attempt that has to time
// out before anything else is tried.
func negotiate(ctx context.Context, host string, creds Credentials, timeout time.Duration, known *discoveryResult) (querier, error) {
	if known != nil {
		return connectAs(host, creds, timeout, known)
	}

	// Ask the device itself first: its answer names the transport, the port and
	// the login version, which removes all the guesswork below.
	if found, err := discoverHost(ctx, host, unicastDiscoveryBudget(timeout)); err == nil {
		if q, err := connectAs(host, creds, timeout, found); err == nil {
			if _, err := q.Read(ctx); err == nil {
				return q, nil
			}
			q.Close()
		}
	}

	// Failures are collected rather than logged as they happen: one unreachable
	// device would otherwise produce a stack of error lines per scrape, none of
	// which is the whole story on its own.
	var attempts []string
	for _, candidate := range probeOrder() {
		q, err := connectAs(host, creds, timeout, candidate)
		if err != nil {
			attempts = append(attempts, fmt.Sprintf("%s: %s", candidate.describe(), err))
			continue
		}
		if _, err := q.Read(ctx); err != nil {
			q.Close()
			attempts = append(attempts, fmt.Sprintf("%s: %s", candidate.describe(), err))
			// A device that answered but rejected the login will reject it just
			// as firmly over every other transport, so there is nothing to gain
			// from trying the rest.
			if errors.Is(err, errKlapUnauthorized) || errors.Is(err, errAESUnauthorized) {
				break
			}
			continue
		}
		return q, nil
	}

	return nil, fmt.Errorf("could not reach %s, tried -- %s", host, strings.Join(attempts, "; "))
}

// unicastDiscoveryBudget returns how long the "ask the device what it speaks"
// step may take out of a device's whole time budget.
//
// It must be a fraction of it, not the lot. Asking one device directly is a
// single UDP round trip, answered on a LAN in milliseconds, whereas the KLAP
// handshake that follows needs two HTTP round trips. Handing the question the
// entire timeout means a device that does not answer it — one on firmware with
// no discovery responder, say — leaves nothing for the connection itself, and
// every transport then fails instantly with a context that expired before it
// was ever tried.
func unicastDiscoveryBudget(timeout time.Duration) time.Duration {
	const share = 4

	budget := timeout / share
	// A floor, so a short device timeout does not shrink the question to
	// something no device could answer in time.
	if budget < 500*time.Millisecond {
		budget = 500 * time.Millisecond
	}
	// ...but never more than the caller allowed in the first place.
	if budget > timeout {
		budget = timeout
	}
	return budget
}

// probeOrder is the sequence of transports tried for a device that did not
// answer discovery, most likely first.
//
// It is a var so tests can point the candidates at a device on a loopback port;
// production code uses the real order returned here.
var probeOrder = func() []*discoveryResult {
	return []*discoveryResult{
		// Newer firmware, which is also the firmware that leaves 9999 closed.
		{EncryptType: encryptKLAP, Family: "SMART", LoginVersion: klapLoginV2},
		// An IOT device whose firmware moved to KLAP. Login version 2 first:
		// it is the build that a version 1 attempt fails on with what reads
		// like a wrong password.
		{EncryptType: encryptKLAP, Family: "IOT", LoginVersion: klapLoginV2},
		{EncryptType: encryptKLAP, Family: "IOT", LoginVersion: klapLoginV1},
		// Tapo-generation firmware from before KLAP.
		{EncryptType: encryptAES, Family: "SMART"},
		// The legacy transport, last for the reason given on negotiate.
		{EncryptType: encryptXOR, Family: "IOT"},
	}
}

// describe names a transport for an error message.
func (d *discoveryResult) describe() string {
	switch d.EncryptType {
	case encryptXOR:
		return "legacy protocol on port 9999"
	case encryptAES:
		return "AES on HTTP"
	case encryptKLAP:
		return fmt.Sprintf("%s over KLAP login version %d", strings.ToLower(string(d.dialect())), d.loginVersion())
	}
	return d.EncryptType
}

// connectAs builds the transport and dialect a discovery result calls for.
func connectAs(host string, creds Credentials, timeout time.Duration, found *discoveryResult) (querier, error) {
	switch found.EncryptType {
	case encryptXOR, "":
		return newIOTQuerier(&xorTransport{host: host, timeout: timeout}), nil

	case encryptKLAP:
		t := &klapTransport{
			client:       httpClient(timeout),
			baseURL:      deviceURL(host, found) + "/app",
			credentials:  creds,
			loginVersion: found.loginVersion(),
		}
		if found.dialect() == ProtocolSMART {
			return newSMARTQuerier(t), nil
		}
		return newIOTQuerier(t), nil

	case encryptAES:
		t := &aesTransport{
			client:      httpClient(timeout),
			baseURL:     deviceURL(host, found) + "/app",
			credentials: creds,
		}
		return newSMARTQuerier(t), nil
	}

	return nil, fmt.Errorf("unsupported encryption scheme %q", found.EncryptType)
}

// deviceURL builds the base URL for an HTTP transport.
func deviceURL(host string, found *discoveryResult) string {
	scheme := "http"
	port := found.port()
	if found.HTTPS {
		scheme = "https"
		if found.HTTPPort == 0 {
			port = 443
		}
	}
	return fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(host, fmt.Sprint(port)))
}

// httpClient builds the HTTP client used by the KLAP and AES transports.
//
// Redirects are refused rather than followed: these are LAN devices answering
// on a fixed path, so a redirect is either a captive portal or something else
// pretending to be a plug, and following it would send the session cookie
// somewhere it does not belong.
func httpClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Connect reaches a single device by address, negotiating the transport it
// speaks. The connection is established lazily on the first read, so a device
// that is switched off at start-up does not hold up the exporter and starts
// reporting as soon as it comes back.
func Connect(host string, creds Credentials, timeout time.Duration) *Device {
	return &Device{host: host, creds: creds, timeout: timeout}
}

// Discover broadcasts to target and returns a device for every answer.
func Discover(ctx context.Context, target string, timeout time.Duration, packets int, creds Credentials, deviceTimeout time.Duration) ([]*Device, error) {
	results, err := discover(ctx, target, timeout, packets)
	if err != nil {
		return nil, err
	}

	devices := make([]*Device, 0, len(results))
	for _, result := range results {
		devices = append(devices, &Device{
			host:       result.Host,
			alias:      result.Alias,
			discovered: result,
			creds:      creds,
			timeout:    deviceTimeout,
		})
	}
	return devices, nil
}
