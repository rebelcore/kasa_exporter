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

package collector

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/rebelcore/kasa_exporter/kasa"
)

// TestMain parses the command line once so the collector flags take their
// declared defaults. Without it every --collector.<name> value is the zero
// value, false, and no collector is enabled at all.
//
// Discovery is turned off and no address is given, which is a configuration
// with nothing to poll. That is deliberate twice over: these tests substitute
// the snapshot rather than talk to devices, so a test suite must not broadcast
// on the LAN of whoever runs it, and it leaves the real snapshot function
// reaching its own failure path for TestSnapshotSurfacesTheRegistryError.
func TestMain(m *testing.M) {
	_, err := kingpin.CommandLine.Parse([]string{"--no-kasa.discovery"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse flags: %v\n", err)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// testLogger discards output; collector tests assert on metrics, not on logs.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// withSnapshot substitutes a fixed set of device readings for the scrape,
// restoring the real one when the test ends.
func withSnapshot(t *testing.T, results []kasa.Result, err error) {
	t.Helper()

	original := snapshot
	snapshot = func(*slog.Logger) ([]kasa.Result, error) { return results, err }
	t.Cleanup(func() { snapshot = original })
}

// collectMetrics runs a collector and renders what it emitted as
// "name{label=value,...}" keyed by value, which keeps the assertions in these
// tests readable.
func collectMetrics(t *testing.T, c Collector) (map[string]float64, error) {
	t.Helper()

	ch := make(chan prometheus.Metric, 256)
	updateErr := c.Update(ch)
	close(ch)

	got := make(map[string]float64)
	for metric := range ch {
		var pb dto.Metric
		if err := metric.Write(&pb); err != nil {
			t.Fatalf("writing metric: %v", err)
		}

		labels := make([]string, 0, len(pb.GetLabel()))
		for _, pair := range pb.GetLabel() {
			labels = append(labels, fmt.Sprintf("%s=%s", pair.GetName(), pair.GetValue()))
		}
		sort.Strings(labels)

		name := metricName(metric.Desc().String())
		key := name
		if len(labels) > 0 {
			key = fmt.Sprintf("%s{%s}", name, strings.Join(labels, ","))
		}

		switch {
		case pb.GetGauge() != nil:
			got[key] = pb.GetGauge().GetValue()
		case pb.GetCounter() != nil:
			got[key] = pb.GetCounter().GetValue()
		default:
			t.Fatalf("metric %s is neither a gauge nor a counter", key)
		}
	}
	return got, updateErr
}

// metricName pulls the fully qualified name out of a descriptor's string form,
// which is the only place the client library exposes it.
func metricName(desc string) string {
	const marker = `fqName: "`
	start := strings.Index(desc, marker)
	if start < 0 {
		return desc
	}
	rest := desc[start+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return desc
	}
	return rest[:end]
}

// wantMetric asserts that a metric was emitted with the expected value.
func wantMetric(t *testing.T, got map[string]float64, key string, want float64) {
	t.Helper()
	value, ok := got[key]
	if !ok {
		t.Errorf("want %s, which was not emitted (got %v)", key, sortedKeys(got))
		return
	}
	if value != want {
		t.Errorf("want %s = %g, got %g", key, want, value)
	}
}

// wantNoMetric asserts that a metric was not emitted.
func wantNoMetric(t *testing.T, got map[string]float64, key string) {
	t.Helper()
	if value, ok := got[key]; ok {
		t.Errorf("want %s not to be emitted, got %g", key, value)
	}
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// float and boolean build the optional values a Reading is made of.
func float(v float64) *float64 { return &v }
func boolean(v bool) *bool     { return &v }

func TestNewKasaCollectorEnablesEveryTypeByDefault(t *testing.T) {
	// Every device kind is reported unless the operator turns it off, so a
	// fleet is fully covered the moment the exporter starts.
	restore := SnapshotCollectorStates()
	defer restore()

	c, err := NewKasaCollector(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"bulb", "device", "dimmer", "hub", "lightstrip", "plug", "strip", "wallswitch"}
	got := make([]string, 0, len(c.Collectors))
	for name := range c.Collectors {
		got = append(got, name)
	}
	sort.Strings(got)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("want collectors %v, got %v", want, got)
	}
}

func TestNewKasaCollectorFilters(t *testing.T) {
	restore := SnapshotCollectorStates()
	defer restore()

	c, err := NewKasaCollector(testLogger(), "plug", "strip")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want, have := 2, len(c.Collectors); want != have {
		t.Fatalf("want %d collectors, have %d", want, have)
	}

	if _, err := NewKasaCollector(testLogger(), "nosuchthing"); err == nil {
		t.Error("want an error for an unknown collector")
	}

	*collectorState["bulb"] = false
	if _, err := NewKasaCollector(testLogger(), "bulb"); err == nil {
		t.Error("want an error for a disabled collector")
	}
}

func TestDisableDefaultCollectors(t *testing.T) {
	restore := SnapshotCollectorStates()
	defer restore()

	// A collector named on the command line stays on, which is what makes
	// --collector.disable-defaults usable as an opt-in switch.
	forcedCollectors["plug"] = true
	DisableDefaultCollectors()

	if !*collectorState["plug"] {
		t.Error("want the forced collector left enabled")
	}
	if *collectorState["bulb"] {
		t.Error("want the unforced collector disabled")
	}
}

func TestKasaCollectorCollect(t *testing.T) {
	restore := SnapshotCollectorStates()
	defer restore()

	withSnapshot(t, []kasa.Result{{
		Host:    "192.168.1.20",
		Alias:   "Rack A",
		Reading: &kasa.Reading{Host: "192.168.1.20", Alias: "Rack A", Type: kasa.TypePlug, On: boolean(true)},
	}}, nil)

	c, err := NewKasaCollector(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _ := collectMetrics(t, collectorFunc(func(ch chan<- prometheus.Metric) error {
		c.Collect(ch)
		return nil
	}))

	// Every enabled collector reports its own duration and success, and a fleet
	// with no bulbs is a success with nothing to report rather than a failure.
	for _, name := range []string{"plug", "bulb", "device", "strip"} {
		wantMetric(t, got, fmt.Sprintf("kasa_scrape_collector_success{collector=%s}", name), 1)
	}
	wantMetric(t, got, "kasa_plug_on{alias=Rack A,host=192.168.1.20}", 1)
}

func TestKasaCollectorDescribe(t *testing.T) {
	restore := SnapshotCollectorStates()
	defer restore()

	c, err := NewKasaCollector(testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ch := make(chan *prometheus.Desc, 8)
	c.Describe(ch)
	close(ch)

	if want, have := 2, len(ch); want != have {
		t.Fatalf("want %d descriptors, have %d", want, have)
	}
}

func TestExecuteRecordsFailure(t *testing.T) {
	ch := make(chan prometheus.Metric, 8)
	execute("broken", collectorFunc(func(chan<- prometheus.Metric) error {
		return errors.New("device unreachable")
	}), ch, testLogger())
	close(ch)

	got := make(map[string]float64)
	for metric := range ch {
		var pb dto.Metric
		if err := metric.Write(&pb); err != nil {
			t.Fatalf("writing metric: %v", err)
		}
		got[metricName(metric.Desc().String())] = pb.GetGauge().GetValue()
	}

	if want, have := 0.0, got["kasa_scrape_collector_success"]; want != have {
		t.Fatalf("want success %g, have %g", want, have)
	}
}

func TestNoDataIsStillAnUnsuccessfulScrape(t *testing.T) {
	// ErrNoData is logged quietly rather than as a failure, but the collector's
	// success metric must still read 0: the exporter reported nothing.
	if !IsNoDataError(fmt.Errorf("wrapped: %w", ErrNoData)) {
		t.Fatal("want a wrapped ErrNoData to be recognised")
	}
	if IsNoDataError(errors.New("something else")) {
		t.Fatal("want an unrelated error not to be recognised")
	}

	ch := make(chan prometheus.Metric, 8)
	execute("empty", collectorFunc(func(chan<- prometheus.Metric) error {
		return ErrNoData
	}), ch, testLogger())
	close(ch)

	for metric := range ch {
		if !strings.Contains(metric.Desc().String(), "collector_success") {
			continue
		}
		var pb dto.Metric
		if err := metric.Write(&pb); err != nil {
			t.Fatalf("writing metric: %v", err)
		}
		if pb.GetGauge().GetValue() != 0 {
			t.Fatalf("want success 0, got %g", pb.GetGauge().GetValue())
		}
	}
}

// collectorFunc adapts a function to the Collector interface.
type collectorFunc func(ch chan<- prometheus.Metric) error

func (f collectorFunc) Update(ch chan<- prometheus.Metric) error { return f(ch) }
