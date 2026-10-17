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
	"time"

	"github.com/xhit/go-str2duration/v2"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// DeviceFlags are the device settings as kong flags, which the exporter embeds
// in its command line (see Flags).
//
// Nothing here is required. A fleet on original firmware needs no credentials
// at all, and with discovery on — the default — it needs no addresses either,
// so the exporter is expected to work with no configuration on a flat network.
type DeviceFlags struct {
	Hosts    []string `name:"kasa.address" env:"KASA_ADDRESS" sep:"none" placeholder:"HOST,HOST" help:"Comma-separated device addresses to poll. Repeat the flag to add more. Devices found by discovery are polled as well."`
	Username string   `name:"kasa.username" env:"KASA_USERNAME" placeholder:"USERNAME" help:"TP-Link account username for devices that require a login."`
	Password string   `name:"kasa.password" env:"KASA_PASSWORD" placeholder:"PASSWORD" help:"TP-Link account password for devices that require a login."`

	Discovery         bool     `name:"kasa.discovery" env:"KASA_DISCOVERY" default:"true" negatable:"" help:"Find devices by broadcasting on the local network."`
	DiscoveryTarget   string   `name:"kasa.discovery-target" env:"KASA_DISCOVERY_TARGET" default:"255.255.255.255" help:"Broadcast address discovery probes are sent to."`
	DiscoveryInterval Duration `name:"kasa.discovery-interval" env:"KASA_DISCOVERY_INTERVAL" default:"5m" help:"How often to sweep the network for new devices."`
	DiscoveryTimeout  Duration `name:"kasa.discovery-timeout" env:"KASA_DISCOVERY_TIMEOUT" default:"5s" help:"How long each discovery sweep waits for devices to answer."`
	DiscoveryPackets  int      `name:"kasa.discovery-packets" env:"KASA_DISCOVERY_PACKETS" default:"3" help:"Number of probes each discovery sweep sends, since broadcast datagrams are lossy."`

	Timeout     Duration `name:"kasa.timeout" env:"KASA_TIMEOUT" default:"5s" help:"How long to wait for a single device to answer."`
	CacheTTL    Duration `name:"kasa.cache-ttl" env:"KASA_CACHE_TTL" default:"10s" help:"How long one set of readings is reused. Shares a single round of device queries across the collectors of a scrape; keep it below the scrape interval."`
	Concurrency int      `name:"kasa.concurrency" env:"KASA_CONCURRENCY" default:"16" help:"Maximum number of devices queried at once."`
	KeepMissing bool     `name:"kasa.keep-missing" env:"KASA_KEEP_MISSING" default:"true" negatable:"" help:"Keep polling devices that have stopped answering discovery, so they report as unreachable rather than disappearing."`
}

// Duration is the type of the duration flags. Besides every unit
// time.ParseDuration takes, it accepts d for days and w for weeks, as the
// kingpin flags it replaced did, so a configuration such as
// KASA_DISCOVERY_INTERVAL=1d keeps working.
type Duration time.Duration

// UnmarshalText parses a flag value such as "5m" or "1d12h". Kong uses it for
// any flag whose type implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := str2duration.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// settings is filled in when the command line is parsed, and read once when
// the registry is built. The variables below point into it.
var (
	settings          DeviceFlags
	deviceHosts       = &settings.Hosts
	username          = &settings.Username
	password          = &settings.Password
	discovery         = &settings.Discovery
	discoveryTarget   = &settings.DiscoveryTarget
	discoveryInterval = &settings.DiscoveryInterval
	discoveryTimeout  = &settings.DiscoveryTimeout
	discoveryPackets  = &settings.DiscoveryPackets
	timeout           = &settings.Timeout
	cacheTTL          = &settings.CacheTTL
	concurrency       = &settings.Concurrency
	keepMissing       = &settings.KeepMissing
)

// Flags returns the device settings for the exporter to embed in its command
// line with kong.Embed; parsing fills them in place.
func Flags() *DeviceFlags {
	return &settings
}

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
		DiscoveryInterval: time.Duration(*discoveryInterval),
		DiscoveryTimeout:  time.Duration(*discoveryTimeout),
		DiscoveryPackets:  *discoveryPackets,
		Timeout:           time.Duration(*timeout),
		CacheTTL:          time.Duration(*cacheTTL),
		Concurrency:       *concurrency,
		KeepMissing:       *keepMissing,
		Logger:            logger,
	}, nil
}

// parseHosts splits and cleans the configured addresses. The flag is repeatable
// and each value may itself be a comma-separated list, and KASA_ADDRESS may
// also hold one address per line, so all of those spellings work; blanks and
// duplicates are dropped so a trailing comma or a repeated address does not
// create a second connection to one device.
func parseHosts(values []string) []string {
	seen := make(map[string]bool)
	var hosts []string
	for _, value := range values {
		for _, host := range strings.FieldsFunc(value, isHostSeparator) {
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

// isHostSeparator reports whether r separates two addresses: a comma, or a
// line break between the values of KASA_ADDRESS.
func isHostSeparator(r rune) bool {
	return r == ',' || r == '\n' || r == '\r'
}
