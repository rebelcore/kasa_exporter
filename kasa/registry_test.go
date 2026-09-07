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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestRegistry builds a registry with discovery off and the given devices
// already installed, so a test can drive the snapshot logic without a network.
func newTestRegistry(t *testing.T, ttl time.Duration, devices map[string]*Device) *Registry {
	t.Helper()

	r := NewRegistry(Options{
		Timeout:     time.Second,
		CacheTTL:    ttl,
		Concurrency: 4,
	})
	r.devices = devices
	return r
}

func TestRegistrySnapshotSharesOneRoundOfQueries(t *testing.T) {
	// Eight collectors run concurrently in one scrape. Without a shared
	// snapshot each would query the fleet itself, and a KLAP handshake already
	// costs two round trips.
	querier := &stubQuerier{reading: &Reading{Alias: "Rack A", Type: TypeStrip}}
	registry := newTestRegistry(t, time.Minute, map[string]*Device{
		"192.168.1.20": {host: "192.168.1.20", querier: querier},
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := registry.Snapshot(t.Context()); len(got) != 1 {
				t.Errorf("want 1 result, got %d", len(got))
			}
		}()
	}
	wg.Wait()

	if querier.reads != 1 {
		t.Fatalf("want the device read once for the whole scrape, got %d", querier.reads)
	}
}

func TestRegistrySnapshotRefreshesAfterTTL(t *testing.T) {
	querier := &stubQuerier{reading: &Reading{Alias: "Rack A"}}
	registry := newTestRegistry(t, time.Nanosecond, map[string]*Device{
		"192.168.1.20": {host: "192.168.1.20", querier: querier},
	})

	registry.Snapshot(t.Context())
	time.Sleep(time.Millisecond)
	registry.Snapshot(t.Context())

	if querier.reads != 2 {
		t.Fatalf("want 2 reads once the cache aged out, got %d", querier.reads)
	}
}

func TestRegistrySnapshotKeepsFailedDevices(t *testing.T) {
	// A device that fails must still appear, under its last known name. One
	// that simply disappeared from the output cannot be alerted on.
	working := &stubQuerier{reading: &Reading{Alias: "Rack A", Type: TypeStrip}}
	failing := &stubQuerier{err: errors.New("connection refused")}

	registry := newTestRegistry(t, time.Minute, map[string]*Device{
		"192.168.1.20": {host: "192.168.1.20", querier: working},
		"192.168.1.21": {host: "192.168.1.21", querier: failing, alias: "Rack B"},
	})

	results := registry.Snapshot(t.Context())
	if want, have := 2, len(results); want != have {
		t.Fatalf("want %d results, have %d", want, have)
	}

	// Results are ordered by address so the metrics are stable between scrapes.
	if want, have := "192.168.1.20", results[0].Host; want != have {
		t.Fatalf("want %q first, have %q", want, have)
	}
	if results[0].Err != nil {
		t.Errorf("want no error for the working device, got %v", results[0].Err)
	}
	if results[1].Err == nil {
		t.Error("want an error for the failing device")
	}
	if want, have := "Rack B", results[1].Alias; want != have {
		t.Errorf("want the failed device reported as %q, have %q", want, have)
	}
	if results[1].Reading != nil {
		t.Error("want no reading for the failing device")
	}
}

func TestRegistryConfiguredHostsAreConnectedLazily(t *testing.T) {
	registry := NewRegistry(Options{
		Hosts:       []string{"192.168.1.20", "192.168.1.21"},
		Timeout:     time.Second,
		CacheTTL:    time.Minute,
		Concurrency: 2,
	})

	if want, have := 2, len(registry.devices); want != have {
		t.Fatalf("want %d devices, have %d", want, have)
	}
	for host, device := range registry.devices {
		if !registry.manual[host] {
			t.Errorf("want %s recorded as a configured host", host)
		}
		if device.querier != nil {
			t.Errorf("want no connection to %s before the first scrape", host)
		}
	}

	registry.Close()
}

func TestRegistryDiscoveryDropsMissingDevices(t *testing.T) {
	// A retired device must stop being exported, or an alert on it keeps firing
	// for hardware that is no longer in the deployment. A configured host is
	// exempt: it is not expected to answer a broadcast at all, which is usually
	// why it was configured.
	registry := NewRegistry(Options{
		Hosts:       []string{"192.168.1.99"},
		Timeout:     time.Second,
		CacheTTL:    time.Minute,
		Concurrency: 2,
		KeepMissing: false,
	})
	discovered := &Device{host: "192.168.1.20", querier: &stubQuerier{reading: &Reading{}}}
	registry.devices["192.168.1.20"] = discovered

	registry.reconcile(nil)

	if _, ok := registry.devices["192.168.1.20"]; ok {
		t.Error("want the discovered device dropped once it stopped answering")
	}
	if _, ok := registry.devices["192.168.1.99"]; !ok {
		t.Error("want the configured host kept")
	}
}

func TestRegistryKeepMissingRetainsDevices(t *testing.T) {
	registry := NewRegistry(Options{
		Timeout:     time.Second,
		CacheTTL:    time.Minute,
		Concurrency: 2,
		KeepMissing: true,
	})
	registry.devices["192.168.1.20"] = &Device{host: "192.168.1.20", querier: &stubQuerier{reading: &Reading{}}}

	registry.reconcile(nil)

	if _, ok := registry.devices["192.168.1.20"]; !ok {
		t.Error("want the device kept so it keeps reporting as unreachable")
	}
}

func TestRegistryDiscoveryAddsDevices(t *testing.T) {
	registry := NewRegistry(Options{
		Timeout:     time.Second,
		CacheTTL:    time.Minute,
		Concurrency: 2,
		KeepMissing: true,
	})

	found := []*Device{{host: "192.168.1.20", alias: "Rack A"}}
	registry.reconcile(found)
	registry.reconcile(found)

	if want, have := 1, len(registry.devices); want != have {
		t.Fatalf("want %d device after two sweeps, have %d", want, have)
	}
	// A device seen again must not replace the one already connected, or every
	// sweep would throw away a working session.
	if registry.devices["192.168.1.20"] != found[0] {
		t.Error("want the existing device kept across sweeps")
	}
}

func TestRegistryDefaultsConcurrency(t *testing.T) {
	registry := NewRegistry(Options{CacheTTL: time.Minute})
	if registry.opts.Concurrency < 1 {
		t.Fatalf("want a concurrency of at least 1, got %d", registry.opts.Concurrency)
	}
	if registry.logger == nil {
		t.Fatal("want a logger even when none was configured")
	}
}

// withDiscovery substitutes the discovery sweep, recording each call and
// returning fixed devices, so the scheduling can be driven without a network.
func withDiscovery(t *testing.T, found []*Device, err error) *int32 {
	t.Helper()

	var calls int32
	original := discoverDevices
	discoverDevices = func(context.Context, string, time.Duration, int, Credentials, time.Duration) ([]*Device, error) {
		atomic.AddInt32(&calls, 1)
		return found, err
	}
	t.Cleanup(func() { discoverDevices = original })
	return &calls
}

// newDiscoveringRegistry builds a registry with discovery on and everything
// else fast enough for a test.
func newDiscoveringRegistry(interval time.Duration) *Registry {
	return NewRegistry(Options{
		Discovery:         true,
		DiscoveryTarget:   "255.255.255.255",
		DiscoveryInterval: interval,
		DiscoveryTimeout:  10 * time.Millisecond,
		DiscoveryPackets:  1,
		Timeout:           10 * time.Millisecond,
		CacheTTL:          time.Nanosecond,
		Concurrency:       2,
		KeepMissing:       true,
	})
}

func TestRegistryFirstDiscoveryBlocks(t *testing.T) {
	// The first sweep has to complete before the first scrape returns, or the
	// exporter's opening output is an empty endpoint.
	found := []*Device{{host: "192.168.1.20", alias: "Rack A", querier: &stubQuerier{reading: &Reading{Alias: "Rack A"}}}}
	calls := withDiscovery(t, found, nil)

	registry := newDiscoveringRegistry(time.Hour)
	results := registry.Snapshot(t.Context())

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("want 1 sweep, got %d", got)
	}
	if want, have := 1, len(results); want != have {
		t.Fatalf("want the discovered device in the first snapshot, have %d results", have)
	}
}

func TestRegistryLaterDiscoveryRunsInTheBackground(t *testing.T) {
	// Later sweeps must not put the discovery timeout in front of a scrape.
	calls := withDiscovery(t, nil, nil)

	registry := newDiscoveringRegistry(time.Nanosecond)
	registry.Snapshot(t.Context())
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("want 1 sweep after the first scrape, got %d", got)
	}

	// The second scrape schedules a sweep rather than waiting on one.
	time.Sleep(time.Millisecond)
	registry.Snapshot(t.Context())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(calls) >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("want the background sweep to run, got %d sweeps", atomic.LoadInt32(calls))
}

func TestRegistryDiscoveryNotDueIsSkipped(t *testing.T) {
	calls := withDiscovery(t, nil, nil)

	registry := newDiscoveringRegistry(time.Hour)
	registry.Snapshot(t.Context())
	time.Sleep(time.Millisecond)
	registry.Snapshot(t.Context())

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("want the second scrape to skip discovery, got %d sweeps", got)
	}
}

func TestRegistryDiscoveryDisabled(t *testing.T) {
	calls := withDiscovery(t, nil, nil)

	registry := NewRegistry(Options{
		Hosts:       []string{"192.168.1.20"},
		Timeout:     10 * time.Millisecond,
		CacheTTL:    time.Nanosecond,
		Concurrency: 1,
	})
	registry.devices["192.168.1.20"] = &Device{host: "192.168.1.20", querier: &stubQuerier{reading: &Reading{}}}
	registry.Snapshot(t.Context())

	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("want no sweep with discovery off, got %d", got)
	}
}

func TestRegistryDiscoveryFailureIsNotRetriedImmediately(t *testing.T) {
	// A network that is down must not turn every scrape into another full
	// discovery timeout.
	calls := withDiscovery(t, nil, errors.New("sending discovery probes: network is unreachable"))

	registry := newDiscoveringRegistry(time.Hour)
	registry.Snapshot(t.Context())
	time.Sleep(time.Millisecond)
	registry.Snapshot(t.Context())

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("want the failed sweep not retried before the interval, got %d sweeps", got)
	}
	if registry.discovering {
		t.Error("want the sweep marked finished after a failure")
	}
}

func TestRegistryDiscoveryDoesNotOverlap(t *testing.T) {
	// A sweep already running must not be started again by a concurrent scrape,
	// or a slow network produces one sweep per scrape indefinitely.
	registry := newDiscoveringRegistry(time.Nanosecond)
	registry.lastDiscovery = time.Now().Add(-time.Hour)
	registry.discovering = true

	calls := withDiscovery(t, nil, nil)
	registry.refreshDevices(t.Context())

	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("want no sweep while one is already running, got %d", got)
	}
}

func TestRegistryCloseClosesEveryDevice(t *testing.T) {
	first := &stubQuerier{reading: &Reading{}}
	second := &stubQuerier{reading: &Reading{}}
	registry := newTestRegistry(t, time.Minute, map[string]*Device{
		"192.168.1.20": {host: "192.168.1.20", querier: first},
		"192.168.1.21": {host: "192.168.1.21", querier: second},
	})

	registry.Close()

	if first.closed != 1 || second.closed != 1 {
		t.Fatalf("want both devices closed, got %d and %d", first.closed, second.closed)
	}
}
