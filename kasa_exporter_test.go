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

package main

import (
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"

	"github.com/rebelcore/kasa_exporter/collector"
	"github.com/rebelcore/kasa_exporter/config"
)

// testArgs configures the exporter against an address that answers nothing.
//
// That is deliberate: these tests are about the HTTP surface — routing,
// collector filtering, the landing page — and an unreachable device exercises
// all of it while also covering the case operators actually hit. The device
// reports as down, every collector still runs, and the readings are cached for
// the whole run so only the first scrape pays for the failed connection.
// Discovery is off so the tests never broadcast on the LAN of whoever runs
// them.
var testArgs = []string{
	"--no-kasa.discovery",
	"--kasa.address", "127.0.0.1",
	"--kasa.timeout", "100ms",
	"--kasa.cache-ttl", "1h",
}

func TestMain(m *testing.M) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if _, err := parseTestFlags(testArgs...); err != nil {
		logger.Error("failed to parse flags", "err", err)
		os.Exit(2)
	}

	os.Exit(m.Run())
}

// parseTestFlags parses args through the exporter's command line, with exit
// stubbed out so --help and --version cannot end the test binary.
func parseTestFlags(args ...string) (*flags, error) {
	var f flags
	return &f, parseFlags(newParser(&f, kong.Exit(func(int) {})), args)
}

// restoreTestFlags undoes a test's parse once it finishes, restoring the
// collector state and the device settings from testArgs. Without it, a test
// that parses the defaults would leave discovery on for the rest of the run.
func restoreTestFlags(t *testing.T) {
	t.Helper()
	restoreCollectors := collector.SnapshotCollectorStates()
	t.Cleanup(func() {
		if _, err := parseTestFlags(testArgs...); err != nil {
			t.Errorf("failed to restore flags: %v", err)
		}
		restoreCollectors()
	})
}

// clearFlagEnv empties every flag's environment variable for the test, so the
// environment the tests run in cannot leak into what they parse.
func clearFlagEnv(t *testing.T) {
	t.Helper()
	var f flags
	for _, group := range newParser(&f).Model.AllFlags(false) {
		for _, flag := range group {
			for _, name := range flag.Envs {
				t.Setenv(name, "")
			}
		}
	}
}

func strconvBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func TestHandler_ServeHTTP_OK(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	prevNewCollector := newKasaCollector
	t.Cleanup(func() { newKasaCollector = prevNewCollector })

	for _, includeExporterMetrics := range []bool{false, true} {
		t.Run("include_exporter_metrics="+strconvBool(includeExporterMetrics), func(t *testing.T) {
			h, err := newHandler(includeExporterMetrics, 0, logger)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://example/metrics", nil)
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("want status %d, have %d. Body:\n%s", http.StatusOK, rr.Code, rr.Body.String())
			}

			body := rr.Body.String()
			// A device that cannot be reached is reported at 0 rather than
			// omitted, which is what makes a failure alertable by name.
			if !strings.Contains(body, "kasa_device_up") {
				t.Fatalf("expected kasa_device_up in response body")
			}
			if !strings.Contains(body, "kasa_scrape_collector_success") {
				t.Fatalf("expected scrape metrics in response body")
			}
		})
	}
}

func TestHandler_ServeHTTP_RejectsCombinedCollectExclude(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := newHandler(false, 0, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example/metrics?collect[]=device&exclude[]=plug", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want status %d, have %d. Body:\n%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestHandler_ServeHTTP_MissingCollector(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := newHandler(false, 0, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example/metrics?collect[]=does_not_exist", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want status %d, have %d. Body:\n%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestHandler_ServeHTTP_CollectAndExclude(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := newHandler(false, 0, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Run("collect", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://example/metrics?collect[]=device", nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("want status %d, have %d. Body:\n%s", http.StatusOK, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, "kasa_device_up") {
			t.Fatalf("expected device metrics in response body")
		}
		if strings.Contains(body, `collector="plug"`) {
			t.Fatalf("did not expect the plug collector to run")
		}
	})

	t.Run("exclude", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://example/metrics?exclude[]=device", nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("want status %d, have %d. Body:\n%s", http.StatusOK, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if strings.Contains(body, "kasa_device_up") {
			t.Fatalf("did not expect device metrics in response body")
		}
		if !strings.Contains(body, `collector="plug"`) {
			t.Fatalf("expected the remaining collectors to run")
		}
	})

	t.Run("exclude an unknown collector", func(t *testing.T) {
		// Warned about but not fatal: excluding an already-disabled collector
		// is a legitimate no-op.
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://example/metrics?exclude[]=does_not_exist", nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("want status %d, have %d. Body:\n%s", http.StatusOK, rr.Code, rr.Body.String())
		}
	})
}

func TestNewHandler_InnerHandlerError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	prev := newKasaCollector
	t.Cleanup(func() { newKasaCollector = prev })

	newKasaCollector = func(*slog.Logger, ...string) (*collector.KasaCollector, error) {
		return nil, errors.New("boom")
	}

	if _, err := newHandler(false, 0, logger); err == nil {
		t.Fatalf("expected error")
	}
}

func TestNewHandler_RegisterError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	prev := registerWithRegistry
	t.Cleanup(func() { registerWithRegistry = prev })

	registerWithRegistry = func(*prometheus.Registry, prometheus.Collector) error {
		return errors.New("boom")
	}

	if _, err := newHandler(false, 0, logger); err == nil {
		t.Fatalf("expected error")
	}
}

func TestRun_BuildsMuxAndCallsListenAndServe(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	prev := listenAndServe
	t.Cleanup(func() { listenAndServe = prev })

	var served *http.Server
	listenAndServe = func(server *http.Server, _ *web.FlagConfig, _ *slog.Logger) error {
		served = server
		return nil
	}

	if err := run("/metrics", false, 0, false, 1, false, &web.FlagConfig{}, logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if served == nil {
		t.Fatal("expected the server to be handed to the listener")
	}

	rr := httptest.NewRecorder()
	served.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://example/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want status %d, have %d", http.StatusOK, rr.Code)
	}
}

func TestRun_MetricsPathRoot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	prev := listenAndServe
	t.Cleanup(func() { listenAndServe = prev })

	var served *http.Server
	listenAndServe = func(server *http.Server, _ *web.FlagConfig, _ *slog.Logger) error {
		served = server
		return nil
	}

	if err := run("/", false, 0, false, 1, false, &web.FlagConfig{}, logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With metrics at the root there is no landing page to serve.
	rr := httptest.NewRecorder()
	served.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://example/", nil))
	if !strings.Contains(rr.Body.String(), "kasa_scrape_collector_success") {
		t.Fatalf("expected metrics at the root, got:\n%s", rr.Body.String())
	}
}

func TestRun_RootUserBranch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	prevUser := currentUser
	prevListen := listenAndServe
	t.Cleanup(func() {
		currentUser = prevUser
		listenAndServe = prevListen
	})

	currentUser = func() (*user.User, error) { return &user.User{Uid: "0"}, nil }
	listenAndServe = func(*http.Server, *web.FlagConfig, *slog.Logger) error { return nil }

	if err := run("/metrics", false, 0, false, 1, false, &web.FlagConfig{}, logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_ListenAndServeError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	prev := listenAndServe
	t.Cleanup(func() { listenAndServe = prev })

	listenAndServe = func(*http.Server, *web.FlagConfig, *slog.Logger) error {
		return errors.New("boom")
	}

	if err := run("/metrics", false, 0, false, 1, false, &web.FlagConfig{}, logger); err == nil {
		t.Fatal("expected error")
	}
}

func TestRun_NilToolkitFlags(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run("/metrics", false, 0, false, 1, false, nil, logger); err == nil {
		t.Fatal("expected error")
	}
}

func TestRun_DisableDefaultCollectorsBranch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	restoreCollectors := collector.SnapshotCollectorStates()
	prev := listenAndServe
	t.Cleanup(func() {
		listenAndServe = prev
		restoreCollectors()
	})

	listenAndServe = func(*http.Server, *web.FlagConfig, *slog.Logger) error { return nil }

	// With every collector off, building the handler must still succeed: the
	// endpoint serves the exporter's own metrics and nothing else.
	if err := run("/metrics", false, 0, true, 1, false, &web.FlagConfig{}, logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_NewHandlerError(t *testing.T) {
	// A handler that cannot be built must stop the exporter with the reason,
	// rather than serving an endpoint that collects nothing.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	prevCollector := newKasaCollector
	prevListen := listenAndServe
	t.Cleanup(func() {
		newKasaCollector = prevCollector
		listenAndServe = prevListen
	})

	newKasaCollector = func(*slog.Logger, ...string) (*collector.KasaCollector, error) {
		return nil, errors.New("boom")
	}
	listenAndServe = func(*http.Server, *web.FlagConfig, *slog.Logger) error { return nil }

	err := run("/metrics", false, 0, false, 1, false, &web.FlagConfig{}, logger)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "metrics handler") {
		t.Fatalf("want the failure named in the error, got %q", err)
	}
}

func TestRun_BuildMuxError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	prev := listenAndServe
	t.Cleanup(func() { listenAndServe = prev })
	listenAndServe = func(*http.Server, *web.FlagConfig, *slog.Logger) error { return nil }

	if err := run("no-leading-slash", false, 0, false, 1, false, &web.FlagConfig{}, logger); err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildMux_LandingPageError(t *testing.T) {
	prev := newLandingPage
	t.Cleanup(func() { newLandingPage = prev })

	newLandingPage = func(web.LandingConfig) (*web.LandingPageHandler, error) {
		return nil, errors.New("boom")
	}

	if _, err := buildMux("/metrics", http.NewServeMux(), false); err == nil {
		t.Fatal("expected error")
	}
}

func TestHandler_InnerHandlerEnabledCollectorsOnce(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	prevNewCollector := newKasaCollector
	t.Cleanup(func() { newKasaCollector = prevNewCollector })

	// Return a fixed collector set regardless of filters, so the test does not
	// depend on the (global, mutable) collector enable/disable state.
	newKasaCollector = func(*slog.Logger, ...string) (*collector.KasaCollector, error) {
		return &collector.KasaCollector{
			Collectors: map[string]collector.Collector{"alpha": nil, "beta": nil, "gamma": nil},
		}, nil
	}

	// newHandler runs innerHandler once with no filters, populating the full
	// enabled-collector set.
	h, err := newHandler(false, 0, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := len(h.enabledCollectors)
	if want == 0 {
		t.Fatal("expected enabledCollectors to be populated")
	}

	// A request that excludes every collector resolves to an empty filter set,
	// driving innerHandler down the no-filter branch again. Hit it concurrently:
	// without the sync.Once guard this races on, and duplicates entries in,
	// h.enabledCollectors (caught here by -race and the length assertion).
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.innerHandler(); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(h.enabledCollectors); got != want {
		t.Fatalf("enabledCollectors changed: want %d entries, got %d (%v)", want, got, h.enabledCollectors)
	}
}

func TestBuildMux_Pprof(t *testing.T) {
	const pprofMarker = "Types of profiles available"

	for _, enabled := range []bool{false, true} {
		t.Run("enabled="+strconvBool(enabled), func(t *testing.T) {
			mux, err := buildMux("/metrics", http.NewServeMux(), enabled)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://example/debug/pprof/", nil)
			mux.ServeHTTP(rr, req)

			hasPprof := strings.Contains(rr.Body.String(), pprofMarker)
			if enabled && !hasPprof {
				t.Fatalf("pprof enabled: expected pprof index at /debug/pprof/, got status %d body:\n%s", rr.Code, rr.Body.String())
			}
			if !enabled && hasPprof {
				t.Fatal("pprof disabled: did not expect pprof index to be served")
			}
		})
	}
}

func TestBuildMux_LandingPageProfilingFollowsFlag(t *testing.T) {
	// The toolkit defaults its Profiling field to "true", which would advertise
	// links that 404 whenever profiling is off.
	for _, enabled := range []bool{false, true} {
		t.Run("enabled="+strconvBool(enabled), func(t *testing.T) {
			mux, err := buildMux("/metrics", http.NewServeMux(), enabled)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://example/", nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("want status %d for the landing page, got %d", http.StatusOK, rr.Code)
			}

			advertised := strings.Contains(rr.Body.String(), "/debug/pprof")
			if advertised != enabled {
				t.Fatalf("pprof links advertised = %v, want %v", advertised, enabled)
			}
		})
	}
}

func TestGitTag(t *testing.T) {
	prev := version.Version
	t.Cleanup(func() { version.Version = prev })

	for _, tt := range []struct{ set, want string }{
		{set: "", want: "unknown"},
		{set: "unknown", want: "unknown"},
		{set: "1.2.3", want: "v1.2.3"},
		{set: "v1.2.3", want: "v1.2.3"},
		{set: " 1.2.3 ", want: "v1.2.3"},
	} {
		version.Version = tt.set
		if got := gitTag(); got != tt.want {
			t.Fatalf("version %q: want %q, got %q", tt.set, tt.want, got)
		}
	}
}

func TestVersionString_IncludesGitTag(t *testing.T) {
	prevVersion := version.Version
	prevBranch := version.Branch
	prevRevision := version.Revision
	t.Cleanup(func() {
		version.Version = prevVersion
		version.Branch = prevBranch
		version.Revision = prevRevision
	})

	version.Version = "1.2.3"
	version.Branch = "master"
	version.Revision = "deadbeef"

	s := versionString("kasa_exporter")
	if !strings.Contains(s, "git=v1.2.3") {
		t.Fatalf("expected version output to include git tag, got:\n%s", s)
	}
}

// TestHandler_ServeHTTP_ExcludeAllCollectors guards the exclude[] semantics: an
// empty filter list means "every enabled collector" to innerHandler, so a query
// that excludes them all must be rejected rather than silently serving the
// complete set — the exact inverse of what was asked for.
func TestHandler_ServeHTTP_ExcludeAllCollectors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	h, err := newHandler(false, 0, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(h.enabledCollectors) == 0 {
		t.Fatal("expected some enabled collectors")
	}

	query := url.Values{}
	for _, c := range h.enabledCollectors {
		query.Add("exclude[]", c)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics?"+query.Encode(), nil))

	if want, have := http.StatusBadRequest, rec.Code; want != have {
		t.Fatalf("want status %d, have %d. Body:\n%s", want, have, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "kasa_device_up") {
		t.Fatalf("excluding every collector still served collector metrics:\n%s", rec.Body.String())
	}
}

// TestValidateMetricsPath covers the values http.ServeMux would panic on, which
// would otherwise crash the exporter at startup instead of returning an error.
func TestValidateMetricsPath(t *testing.T) {
	for _, tc := range []struct {
		name        string
		path        string
		enablePprof bool
		wantErr     bool
	}{
		{name: "default", path: "/metrics"},
		{name: "root", path: "/"},
		{name: "nested", path: "/a/b/metrics"},
		{name: "pprof disabled allows pprof path", path: "/debug/pprof/x"},
		{name: "empty", path: "", wantErr: true},
		{name: "missing leading slash", path: "metrics", wantErr: true},
		{name: "leading space", path: " /metrics", wantErr: true},
		{name: "embedded space", path: "/met rics", wantErr: true},
		{name: "wildcard", path: "/m/{id}", wantErr: true},
		{name: "pprof conflict", path: "/debug/pprof/", enablePprof: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMetricsPath(tc.path, tc.enablePprof)
			if tc.wantErr && err == nil {
				t.Fatalf("want error for %q, got none", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.path, err)
			}
			// A path validateMetricsPath accepts must not panic the mux.
			if err == nil {
				if _, err := buildMux(tc.path, http.NotFoundHandler(), tc.enablePprof); err != nil {
					t.Fatalf("buildMux rejected accepted path %q: %v", tc.path, err)
				}
			}
		})
	}
}

func TestParseFlags_Defaults(t *testing.T) {
	restoreTestFlags(t)
	// Empty environment variables count as unset, so every flag below keeps
	// its default.
	clearFlagEnv(t)

	f, err := parseTestFlags()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := flags{
		MetricsPath:        "/metrics",
		MaxRequests:        40,
		MaxProcs:           1,
		WebListenAddresses: []string{":9498"},
		LogLevel:           "info",
		LogFormat:          "logfmt",
	}
	if !reflect.DeepEqual(*f, want) {
		t.Fatalf("want defaults %+v, have %+v", want, *f)
	}

	// With no configuration at all, discovery finds the devices.
	wantDevices := config.DeviceFlags{
		Discovery:         true,
		DiscoveryTarget:   "255.255.255.255",
		DiscoveryInterval: config.Duration(5 * time.Minute),
		DiscoveryTimeout:  config.Duration(5 * time.Second),
		DiscoveryPackets:  3,
		Timeout:           config.Duration(5 * time.Second),
		CacheTTL:          config.Duration(10 * time.Second),
		Concurrency:       16,
		KeepMissing:       true,
	}
	if have := *config.Flags(); !reflect.DeepEqual(have, wantDevices) {
		t.Fatalf("want device defaults %+v, have %+v", wantDevices, have)
	}
}

func TestParseFlags_Environment(t *testing.T) {
	restoreTestFlags(t)
	clearFlagEnv(t)
	t.Setenv("KASA_ADDRESS", "192.168.1.20,192.168.1.21")
	t.Setenv("KASA_USERNAME", "user@example.com")
	t.Setenv("KASA_DISCOVERY", "false")
	t.Setenv("KASA_DISCOVERY_INTERVAL", "1d")
	t.Setenv("KASA_CONCURRENCY", "4")
	t.Setenv("GOMAXPROCS", "4")

	f, err := parseTestFlags()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := 4, f.MaxProcs; want != have {
		t.Fatalf("want --runtime.gomaxprocs %d from GOMAXPROCS, have %d", want, have)
	}

	devices := config.Flags()
	// The address list is split when the registry is built, so it arrives as
	// given.
	if want, have := []string{"192.168.1.20,192.168.1.21"}, devices.Hosts; !slices.Equal(want, have) {
		t.Errorf("want addresses %q, have %q", want, have)
	}
	if want, have := "user@example.com", devices.Username; want != have {
		t.Errorf("want username %q, have %q", want, have)
	}
	if devices.Discovery {
		t.Error("want discovery off from KASA_DISCOVERY")
	}
	if want, have := config.Duration(24*time.Hour), devices.DiscoveryInterval; want != have {
		t.Errorf("want discovery interval %s, have %s", time.Duration(want), time.Duration(have))
	}
	if want, have := 4, devices.Concurrency; want != have {
		t.Errorf("want concurrency %d, have %d", want, have)
	}
}

func TestParseFlags_ListenAddresses(t *testing.T) {
	restoreTestFlags(t)

	// The flag repeats for several addresses, and a comma is not a separator.
	f, err := parseTestFlags("--web.listen-address", ":9100", "--web.listen-address=[::1]:9100,:9101")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{":9100", "[::1]:9100,:9101"}
	if have := *f.webFlags().WebListenAddresses; !slices.Equal(have, want) {
		t.Fatalf("want listen addresses %q, have %q", want, have)
	}
}

func TestParseFlags_CollectorToggles(t *testing.T) {
	restoreTestFlags(t)

	f, err := parseTestFlags("--collector.disable-defaults", "--collector.plug", "--no-collector.strip")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !f.DisableDefaultCollectors {
		t.Fatal("want --collector.disable-defaults set")
	}

	// run applies --collector.disable-defaults like this; the collectors named
	// on the command line keep the state they were given.
	collector.DisableDefaultCollectors()
	nc, err := collector.NewKasaCollector(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if have := slices.Sorted(maps.Keys(nc.Collectors)); !slices.Equal(have, []string{"plug"}) {
		t.Fatalf("want only plug enabled, have %q", have)
	}
}

func TestParseFlags_RejectsUnknownLogLevel(t *testing.T) {
	restoreTestFlags(t)

	if _, err := parseTestFlags("--log.level", "verbose"); err == nil {
		t.Fatal("want an error for an unknown --log.level")
	}
}

func TestParseFlags_SystemdSocketIsLinuxOnly(t *testing.T) {
	restoreTestFlags(t)

	f, err := parseTestFlags("--web.systemd-socket")
	if runtime.GOOS != "linux" {
		if err == nil {
			t.Fatal("want --web.systemd-socket rejected off Linux")
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !*f.webFlags().WebSystemdSocket {
		t.Fatal("want --web.systemd-socket set")
	}
}

func TestParseFlags_Version(t *testing.T) {
	restoreTestFlags(t)

	var out strings.Builder
	exitCode := -1
	var f flags
	parser := newParser(&f, kong.Writers(&out, io.Discard), kong.Exit(func(code int) { exitCode = code }))
	if err := parseFlags(parser, []string{"--version"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("want exit code 0, have %d", exitCode)
	}
	if want, have := versionString("kasa_exporter")+"\n", out.String(); want != have {
		t.Fatalf("want version output %q, have %q", want, have)
	}
}

func TestParseFlags_HelpListsEveryFlag(t *testing.T) {
	restoreTestFlags(t)

	var out strings.Builder
	var f flags
	parser := newParser(&f, kong.Writers(&out, io.Discard), kong.Exit(func(int) {}))
	if err := parseFlags(parser, []string{"--help"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{
		"--web.telemetry-path", "--[no-]web.disable-exporter-metrics", "--web.max-requests",
		"--[no-]collector.disable-defaults", "--runtime.gomaxprocs", "--[no-]web.enable-pprof",
		"--web.listen-address", "--web.config.file", "--log.level", "--log.format", "--version",
		"--kasa.address", "--kasa.username", "--kasa.password", "--[no-]kasa.discovery",
		"--kasa.discovery-target", "--kasa.discovery-interval", "--kasa.discovery-timeout",
		"--kasa.discovery-packets", "--kasa.timeout", "--kasa.cache-ttl", "--kasa.concurrency",
		"--[no-]kasa.keep-missing",
	}
	for _, name := range []string{"bulb", "device", "dimmer", "hub", "lightstrip", "plug", "strip", "wallswitch"} {
		want = append(want, "--[no-]collector."+name)
	}
	if runtime.GOOS == "linux" {
		want = append(want, "--[no-]web.systemd-socket")
	}
	for _, flag := range want {
		if !strings.Contains(out.String(), flag) {
			t.Errorf("--help does not list %s", flag)
		}
	}
}

func TestFlags_LogConfig(t *testing.T) {
	f := flags{LogLevel: "debug", LogFormat: "json"}
	cfg, err := f.logConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Level.String() != "debug" || cfg.Format.String() != "json" {
		t.Fatalf("want level debug and format json, have %s and %s", cfg.Level, cfg.Format)
	}

	for _, bad := range []flags{
		{LogLevel: "verbose", LogFormat: "logfmt"},
		{LogLevel: "info", LogFormat: "xml"},
	} {
		if _, err := bad.logConfig(); err == nil {
			t.Errorf("want an error for log level %q and format %q", bad.LogLevel, bad.LogFormat)
		}
	}
}
