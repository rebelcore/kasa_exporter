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
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Options configures a Registry.
type Options struct {
	// Hosts are addresses configured by hand. They are never dropped, whether
	// or not they answer discovery — being unreachable by broadcast is the
	// usual reason for configuring one.
	Hosts []string
	// Credentials is the TP-Link login for devices that require one.
	Credentials Credentials

	// Discovery controls the LAN broadcast. Target is the broadcast address,
	// Interval how often a fresh sweep runs, Timeout how long each sweep waits
	// for answers, and Packets how many probes each sweep sends.
	Discovery         bool
	DiscoveryTarget   string
	DiscoveryInterval time.Duration
	DiscoveryTimeout  time.Duration
	DiscoveryPackets  int

	// Timeout bounds a single device operation.
	Timeout time.Duration
	// CacheTTL is how long one set of readings is served for. It exists to
	// share a single round of device queries across the collectors of one
	// scrape, not to serve stale data: keep it well under the scrape interval.
	CacheTTL time.Duration
	// Concurrency caps how many devices are read at once.
	Concurrency int
	// KeepMissing keeps a device that has stopped answering discovery, so it
	// keeps reporting as unreachable instead of silently disappearing.
	KeepMissing bool

	Logger *slog.Logger
}

// Result is one device's outcome for a scrape: a reading, or the error that
// prevented one. A failed device still appears, so it can be reported as
// unreachable by name rather than vanishing from the metrics.
//
// Alias and Type are the device's last known name and category, so a device
// that fails keeps both instead of moving to a new series the moment it goes
// down.
type Result struct {
	Host    string
	Alias   string
	Type    DeviceType
	Reading *Reading
	Err     error
}

// Registry owns the fleet: which devices exist, the connections to them, and
// the readings of the current scrape.
//
// Every collector reads the same snapshot. Without that, a scrape would query
// each device once per collector — eight times over for a fleet where a single
// KLAP handshake already costs two round trips.
type Registry struct {
	opts   Options
	logger *slog.Logger

	// devicesMtx guards the device set and the discovery bookkeeping.
	devicesMtx    sync.Mutex
	devices       map[string]*Device
	manual        map[string]bool
	lastDiscovery time.Time
	discovering   bool

	// snapshotMtx serialises refreshes so that the collectors of one scrape
	// share a single round of device queries rather than racing to start their
	// own.
	snapshotMtx  sync.Mutex
	snapshot     []Result
	snapshotTime time.Time
}

// NewRegistry builds a registry over the configured hosts. Discovery, if
// enabled, runs on the first scrape rather than here, so start-up never blocks
// on the network.
func NewRegistry(opts Options) *Registry {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}

	r := &Registry{
		opts:    opts,
		logger:  opts.Logger,
		devices: make(map[string]*Device, len(opts.Hosts)),
		manual:  make(map[string]bool, len(opts.Hosts)),
	}
	for _, host := range opts.Hosts {
		r.devices[host] = Connect(host, opts.Credentials, opts.Timeout)
		r.manual[host] = true
	}
	return r
}

// Snapshot returns the readings for the current scrape, refreshing them when
// the cached set has aged past the configured TTL.
func (r *Registry) Snapshot(ctx context.Context) []Result {
	r.snapshotMtx.Lock()
	defer r.snapshotMtx.Unlock()

	if time.Since(r.snapshotTime) < r.opts.CacheTTL && r.snapshot != nil {
		return r.snapshot
	}

	r.refreshDevices(ctx)
	r.snapshot = r.readAll(ctx)
	r.snapshotTime = time.Now()
	return r.snapshot
}

// Close releases every device connection.
func (r *Registry) Close() {
	r.devicesMtx.Lock()
	defer r.devicesMtx.Unlock()
	for _, device := range r.devices {
		device.Close()
	}
}

// refreshDevices runs discovery when it is due.
//
// The first sweep blocks, because a scrape that reported nothing at all would
// otherwise be the exporter's first output. Later sweeps run in the background:
// discovery waits a fixed timeout for answers, and paying that on a scrape
// would add it to every collection that happens to fall due at the same time.
func (r *Registry) refreshDevices(ctx context.Context) {
	if !r.opts.Discovery {
		return
	}

	r.devicesMtx.Lock()
	first := r.lastDiscovery.IsZero()
	due := time.Since(r.lastDiscovery) >= r.opts.DiscoveryInterval
	busy := r.discovering
	if due && !busy {
		r.discovering = true
	}
	r.devicesMtx.Unlock()

	if !due || busy {
		return
	}

	if first {
		r.runDiscovery(ctx)
		return
	}
	// Detached from the scrape's context so a finished scrape does not cancel
	// the sweep it started.
	go func() {
		background, cancel := context.WithTimeout(context.Background(), r.opts.DiscoveryTimeout+r.opts.Timeout)
		defer cancel()
		r.runDiscovery(background)
	}()
}

// discoverDevices indirects over Discover so tests can drive the scheduling
// without a network; production code uses the real implementation assigned here.
var discoverDevices = Discover

// runDiscovery performs one sweep and reconciles the results into the device
// set.
//
// A failed sweep still records the time, so a network that is down does not
// turn every scrape into another full discovery timeout. The next attempt comes
// at the normal interval.
func (r *Registry) runDiscovery(ctx context.Context) {
	defer func() {
		r.devicesMtx.Lock()
		r.discovering = false
		r.lastDiscovery = time.Now()
		r.devicesMtx.Unlock()
	}()

	found, err := discoverDevices(ctx, r.opts.DiscoveryTarget, r.opts.DiscoveryTimeout, r.opts.DiscoveryPackets, r.opts.Credentials, r.opts.Timeout)
	if err != nil {
		r.logger.Error("Device discovery failed", "err", err)
		return
	}
	r.reconcile(found)
}

// reconcile merges one sweep's results into the device set: new devices are
// added, and devices that have gone away are dropped when the operator asked
// for that.
func (r *Registry) reconcile(found []*Device) {
	r.devicesMtx.Lock()
	defer r.devicesMtx.Unlock()

	seen := make(map[string]bool, len(found))
	added := 0
	for _, device := range found {
		seen[device.host] = true
		// A device already known keeps the connection it has: replacing it on
		// every sweep would throw away a working session, and with it the KLAP
		// handshake that established it.
		if _, ok := r.devices[device.host]; ok {
			continue
		}
		r.devices[device.host] = device
		added++
		r.logger.Info("Discovered device", "host", device.host, "alias", device.Alias())
	}

	if !r.opts.KeepMissing {
		for host, device := range r.devices {
			// A configured host is not expected to appear in a broadcast sweep
			// at all — that is usually why it was configured — so dropping one
			// here would delete the entire fleet of a manually configured
			// deployment on the first sweep.
			if seen[host] || r.manual[host] {
				continue
			}
			device.Close()
			delete(r.devices, host)
			r.logger.Warn("Device no longer answers discovery, dropping it", "host", host, "alias", device.Alias())
		}
	}

	if added > 0 {
		r.logger.Info("Discovery complete", "devices", len(r.devices), "new", added)
	} else {
		r.logger.Debug("Discovery complete", "devices", len(r.devices))
	}
}

// readAll reads every device, bounded by the configured concurrency, and
// returns the results ordered by address so the metrics are stable between
// scrapes.
func (r *Registry) readAll(ctx context.Context) []Result {
	r.devicesMtx.Lock()
	devices := make([]*Device, 0, len(r.devices))
	for _, device := range r.devices {
		devices = append(devices, device)
	}
	r.devicesMtx.Unlock()

	results := make([]Result, len(devices))
	limit := make(chan struct{}, r.opts.Concurrency)
	var wg sync.WaitGroup

	for i, device := range devices {
		wg.Add(1)
		go func(i int, device *Device) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()

			// Each device gets its own budget so one slow device cannot consume
			// the time the rest of the fleet needs.
			deviceCtx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
			defer cancel()

			reading, err := device.Read(deviceCtx)
			results[i] = Result{
				Host:    device.Host(),
				Alias:   device.Alias(),
				Type:    device.Type(),
				Reading: reading,
				Err:     err,
			}
			if err != nil {
				r.logger.Debug("Failed to read device", "host", device.Host(), "err", err)
			}
		}(i, device)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].Host < results[j].Host })
	return results
}
