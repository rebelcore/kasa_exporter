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

package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
)

// TestMain parses the command line once so the flags take their declared
// defaults rather than the zero values they hold before parsing.
func TestMain(m *testing.M) {
	if err := parseFlags(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse flags: %v\n", err)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// parseFlags parses args against the device flags, as the exporter's command
// line does. Every parse starts from the defaults, so nothing an earlier parse
// set carries over.
func parseFlags(args ...string) error {
	parser, err := kong.New(&struct{}{}, kong.Embed(Flags()))
	if err != nil {
		return err
	}
	_, err = parser.Parse(args)
	return err
}

// restoreFlags puts the flag values back as they were once the test ends,
// since they are process-wide.
func restoreFlags(t *testing.T) {
	t.Helper()
	saved := settings
	t.Cleanup(func() { settings = saved })
}

// setFlags parses args for one test, restoring the previous values afterwards.
func setFlags(t *testing.T, args ...string) {
	t.Helper()
	restoreFlags(t)
	if err := parseFlags(args...); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
}

func TestParseHosts(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   []string
	}{
		{name: "none", values: nil, want: nil},
		{name: "single", values: []string{"192.168.1.20"}, want: []string{"192.168.1.20"}},
		{
			// The flag is repeatable and each value may itself be a list, so
			// both spellings have to work.
			name:   "comma separated",
			values: []string{"192.168.1.20,192.168.1.21"},
			want:   []string{"192.168.1.20", "192.168.1.21"},
		},
		{
			name:   "repeated flag",
			values: []string{"192.168.1.20", "192.168.1.21"},
			want:   []string{"192.168.1.20", "192.168.1.21"},
		},
		{
			name:   "whitespace and a trailing comma",
			values: []string{" 192.168.1.20 , 192.168.1.21 ,"},
			want:   []string{"192.168.1.20", "192.168.1.21"},
		},
		{
			// A repeated address would otherwise open a second connection to
			// one device and export it twice.
			name:   "duplicates",
			values: []string{"192.168.1.20,192.168.1.20", "192.168.1.20"},
			want:   []string{"192.168.1.20"},
		},
		{
			// KASA_ADDRESS may hold one address per line.
			name:   "line separated",
			values: []string{"192.168.1.20\n192.168.1.21\r\nplug.lan\n"},
			want:   []string{"192.168.1.20", "192.168.1.21", "plug.lan"},
		},
		{name: "blank", values: []string{"", " ", ","}, want: nil},
		{name: "hostnames", values: []string{"plug.lan, strip.lan"}, want: []string{"plug.lan", "strip.lan"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHosts(tt.values)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("want %v, got %v", tt.want, got)
			}
		})
	}
}

func TestBuildOptionsDefaults(t *testing.T) {
	// The exporter is expected to work with no configuration at all on a flat
	// network: discovery on, no credentials, no addresses.
	opts, err := buildOptions(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !opts.Discovery {
		t.Error("want discovery enabled by default")
	}
	if want, have := "255.255.255.255", opts.DiscoveryTarget; want != have {
		t.Errorf("want target %q, have %q", want, have)
	}
	if want, have := 5*time.Minute, opts.DiscoveryInterval; want != have {
		t.Errorf("want interval %s, have %s", want, have)
	}
	if !opts.KeepMissing {
		t.Error("want missing devices kept by default, so a failure stays alertable")
	}
	if opts.CacheTTL <= 0 {
		t.Errorf("want a positive cache TTL, have %s", opts.CacheTTL)
	}
	if opts.Concurrency < 1 {
		t.Errorf("want a concurrency of at least 1, have %d", opts.Concurrency)
	}
}

func TestBuildOptionsFromFlags(t *testing.T) {
	setFlags(t,
		"--kasa.address", "192.168.1.20,192.168.1.21",
		"--kasa.address", "plug.lan",
		"--kasa.username", "user@example.com",
		"--kasa.password", "secret",
		"--kasa.discovery-interval", "30s",
		"--kasa.timeout", "2s",
		"--kasa.concurrency", "32",
		"--no-kasa.keep-missing",
	)

	opts, err := buildOptions(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want, have := 3, len(opts.Hosts); want != have {
		t.Fatalf("want %d hosts, have %d: %v", want, have, opts.Hosts)
	}
	if want, have := "user@example.com", opts.Credentials.Username; want != have {
		t.Errorf("want username %q, have %q", want, have)
	}
	if want, have := "secret", opts.Credentials.Password; want != have {
		t.Errorf("want the password passed through, have %q", have)
	}
	if want, have := 30*time.Second, opts.DiscoveryInterval; want != have {
		t.Errorf("want interval %s, have %s", want, have)
	}
	if want, have := 2*time.Second, opts.Timeout; want != have {
		t.Errorf("want timeout %s, have %s", want, have)
	}
	if want, have := 32, opts.Concurrency; want != have {
		t.Errorf("want concurrency %d, have %d", want, have)
	}
	if opts.KeepMissing {
		t.Error("want missing devices dropped")
	}
}

func TestBuildOptionsFromEnvironment(t *testing.T) {
	t.Setenv("KASA_ADDRESS", "192.168.1.20,192.168.1.21\nplug.lan")
	t.Setenv("KASA_USERNAME", "user@example.com")
	t.Setenv("KASA_DISCOVERY", "false")
	t.Setenv("KASA_TIMEOUT", "2s")
	setFlags(t)

	opts, err := buildOptions(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want, have := "192.168.1.20,192.168.1.21,plug.lan", strings.Join(opts.Hosts, ","); want != have {
		t.Errorf("want hosts %s, have %s", want, have)
	}
	if want, have := "user@example.com", opts.Credentials.Username; want != have {
		t.Errorf("want username %q, have %q", want, have)
	}
	if opts.Discovery {
		t.Error("want discovery disabled")
	}
	if want, have := 2*time.Second, opts.Timeout; want != have {
		t.Errorf("want timeout %s, have %s", want, have)
	}

	// A flag overrides its environment variable.
	setFlags(t, "--kasa.discovery", "--kasa.timeout", "3s")
	opts, err = buildOptions(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !opts.Discovery {
		t.Error("want --kasa.discovery to override KASA_DISCOVERY")
	}
	if want, have := 3*time.Second, opts.Timeout; want != have {
		t.Errorf("want timeout %s, have %s", want, have)
	}
}

func TestDurationFlags(t *testing.T) {
	// Days and weeks work as well as every unit time.ParseDuration takes, as
	// they did with kingpin.
	setFlags(t,
		"--kasa.discovery-interval", "1w",
		"--kasa.discovery-timeout", "1.5s",
		"--kasa.timeout", "1d12h",
		"--kasa.cache-ttl", "500ms",
	)

	opts, err := buildOptions(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for name, d := range map[string]struct{ want, have time.Duration }{
		"discovery interval": {7 * 24 * time.Hour, opts.DiscoveryInterval},
		"discovery timeout":  {1500 * time.Millisecond, opts.DiscoveryTimeout},
		"timeout":            {36 * time.Hour, opts.Timeout},
		"cache TTL":          {500 * time.Millisecond, opts.CacheTTL},
	} {
		if d.want != d.have {
			t.Errorf("want %s %s, have %s", name, d.want, d.have)
		}
	}

	for _, value := range []string{"soon", "5", "1y"} {
		if err := parseFlags("--kasa.timeout", value); err == nil {
			t.Errorf("want --kasa.timeout %q rejected", value)
		}
	}
}

func TestBuildOptionsRejectsUnworkableConfigurations(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			// Discovery off with no addresses leaves nothing to poll, which
			// would otherwise be a silently empty endpoint.
			name: "nothing to poll",
			args: []string{"--no-kasa.discovery"},
			want: "no devices to poll",
		},
		{
			name: "no concurrency",
			args: []string{"--kasa.concurrency", "0"},
			want: "concurrency",
		},
		{
			name: "no discovery packets",
			args: []string{"--kasa.discovery-packets", "0"},
			want: "discovery-packets",
		},
		{
			name: "no timeout",
			args: []string{"--kasa.timeout", "0s"},
			want: "timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setFlags(t, tt.args...)

			_, err := buildOptions(testLogger())
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want the error to mention %q, got %q", tt.want, err)
			}
		})
	}
}

func TestBuildOptionsAcceptsExplicitHostsWithoutDiscovery(t *testing.T) {
	setFlags(t, "--no-kasa.discovery", "--kasa.address", "192.168.1.20")

	opts, err := buildOptions(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Discovery {
		t.Error("want discovery disabled")
	}
	if want, have := 1, len(opts.Hosts); want != have {
		t.Fatalf("want %d host, have %d", want, have)
	}
}

func TestBuildRegistry(t *testing.T) {
	// The registry is built lazily so a configuration error surfaces as a
	// failed scrape with the reason in it, rather than as a process that exits
	// before it has served anything.
	registry, err := buildRegistry(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if registry == nil {
		t.Fatal("want a registry")
	}
	registry.Close()

	setFlags(t, "--no-kasa.discovery")
	if _, err := buildRegistry(testLogger()); err == nil {
		t.Fatal("want an error when there is nothing to poll")
	}
}

func TestRegistryIsShared(t *testing.T) {
	// Every collector asks for the registry. They must all get the same one, or
	// each would hold its own connections to the same devices.
	first, err := Registry(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := Registry(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first != second {
		t.Fatal("want the same registry on every call")
	}
	first.Close()
}
