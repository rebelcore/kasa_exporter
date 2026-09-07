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

// Package config exposes the device settings — which devices to talk to, the
// TP-Link login to use, and how long to wait for them — as command-line flags
// and environment variables, and builds the device registry the collectors
// share.
package config

import (
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/alecthomas/kingpin/v2"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// Flag values, bound during kingpin parsing and read once when the registry is
// built.
//
// Nothing here is required. A fleet on original firmware needs no credentials
// at all, and with discovery on — the default — it needs no addresses either,
// so the exporter is expected to work with no configuration on a flat network.
var (
	deviceHosts = kingpin.Flag(
		"kasa.address",
		"Comma-separated device addresses to poll. Repeat the flag to add more. Devices found by discovery are polled as well.",
	).Envar("KASA_ADDRESS").PlaceHolder("HOST,HOST").Strings()
	username = kingpin.Flag(
		"kasa.username",
		"TP-Link account username for devices that require a login.",
	).Envar("KASA_USERNAME").PlaceHolder("USERNAME").String()
	password = kingpin.Flag(
		"kasa.password",
		"TP-Link account password for devices that require a login.",
	).Envar("KASA_PASSWORD").PlaceHolder("PASSWORD").String()

	discovery = kingpin.Flag(
		"kasa.discovery",
		"Find devices by broadcasting on the local network.",
	).Envar("KASA_DISCOVERY").Default("true").Bool()
	discoveryTarget = kingpin.Flag(
		"kasa.discovery-target",
		"Broadcast address discovery probes are sent to.",
	).Envar("KASA_DISCOVERY_TARGET").Default("255.255.255.255").String()
	discoveryInterval = kingpin.Flag(
		"kasa.discovery-interval",
		"How often to sweep the network for new devices.",
	).Envar("KASA_DISCOVERY_INTERVAL").Default("5m").Duration()
	discoveryTimeout = kingpin.Flag(
		"kasa.discovery-timeout",
		"How long each discovery sweep waits for devices to answer.",
	).Envar("KASA_DISCOVERY_TIMEOUT").Default("5s").Duration()
	discoveryPackets = kingpin.Flag(
		"kasa.discovery-packets",
		"Number of probes each discovery sweep sends, since broadcast datagrams are lossy.",
	).Envar("KASA_DISCOVERY_PACKETS").Default("3").Int()

	timeout = kingpin.Flag(
		"kasa.timeout",
		"How long to wait for a single device to answer.",
	).Envar("KASA_TIMEOUT").Default("5s").Duration()
	cacheTTL = kingpin.Flag(
		"kasa.cache-ttl",
		"How long one set of readings is reused. Shares a single round of device queries across the collectors of a scrape; keep it below the scrape interval.",
	).Envar("KASA_CACHE_TTL").Default("10s").Duration()
	concurrency = kingpin.Flag(
		"kasa.concurrency",
		"Maximum number of devices queried at once.",
	).Envar("KASA_CONCURRENCY").Default("16").Int()
	keepMissing = kingpin.Flag(
		"kasa.keep-missing",
		"Keep polling devices that have stopped answering discovery, so they report as unreachable rather than disappearing.",
	).Envar("KASA_KEEP_MISSING").Default("true").Bool()
)

// registryOnce guards the shared registry: every collector asks for it, and
// they must all get the same one or each would hold its own connections to the
// same devices.
var (
	registryOnce sync.Once
	registry     *kasa.Registry
	registryErr  error
)

// Registry returns the shared device registry, building it on first use.
//
// It is built lazily rather than at start-up so that a configuration error
// surfaces as a failed scrape, with the reason in the response, instead of as a
// process that exits before it has served anything.
func Registry(logger *slog.Logger) (*kasa.Registry, error) {
	registryOnce.Do(func() { registry, registryErr = buildRegistry(logger) })
	return registry, registryErr
}

// buildRegistry validates the configuration and builds the registry from it.
// It is separate from Registry so it can be exercised for both outcomes; the
// Once above means only the first result is ever kept.
func buildRegistry(logger *slog.Logger) (*kasa.Registry, error) {
	opts, err := buildOptions(logger)
	if err != nil {
		return nil, err
	}
	return kasa.NewRegistry(opts), nil
}

// buildOptions turns the parsed flags into registry options, rejecting a
// configuration that could not work.
func buildOptions(logger *slog.Logger) (kasa.Options, error) {
	hosts := parseHosts(*deviceHosts)

	// With discovery off and no addresses there is nothing to poll, which would
	// otherwise be a silently empty metrics endpoint.
	if !*discovery && len(hosts) == 0 {
		return kasa.Options{}, errors.New("no devices to poll: enable --kasa.discovery or set --kasa.address")
	}
	if *concurrency < 1 {
		return kasa.Options{}, errors.New("--kasa.concurrency must be at least 1")
	}
	if *discoveryPackets < 1 {
		return kasa.Options{}, errors.New("--kasa.discovery-packets must be at least 1")
	}
	if *timeout <= 0 {
		return kasa.Options{}, errors.New("--kasa.timeout must be positive")
	}

	logger.Debug("Device configuration",
		"hosts", len(hosts),
		"discovery", *discovery,
		"credentials", *username != "",
	)

	return kasa.Options{
		Hosts:             hosts,
		Credentials:       kasa.Credentials{Username: *username, Password: *password},
		Discovery:         *discovery,
		DiscoveryTarget:   *discoveryTarget,
		DiscoveryInterval: *discoveryInterval,
		DiscoveryTimeout:  *discoveryTimeout,
		DiscoveryPackets:  *discoveryPackets,
		Timeout:           *timeout,
		CacheTTL:          *cacheTTL,
		Concurrency:       *concurrency,
		KeepMissing:       *keepMissing,
		Logger:            logger,
	}, nil
}

// parseHosts splits and cleans the configured addresses. The flag is repeatable
// and each value may itself be a comma-separated list, so both spellings work;
// blanks and duplicates are dropped so a trailing comma or a repeated address
// does not create a second connection to one device.
func parseHosts(values []string) []string {
	seen := make(map[string]bool)
	var hosts []string
	for _, value := range values {
		for _, host := range strings.Split(value, ",") {
			host = strings.TrimSpace(host)
			if host == "" || seen[host] {
				continue
			}
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	return hosts
}
