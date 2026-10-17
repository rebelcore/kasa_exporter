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
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// This file covers the branches the per-collector tests do not reach: the
// registry's own bookkeeping, and what each collector does when the device
// registry itself cannot be built.

func TestEveryCollectorSurfacesARegistryFailure(t *testing.T) {
	// A configuration that leaves nothing to poll must fail the scrape with the
	// reason in it, not report an empty but healthy fleet.
	withSnapshot(t, nil, errors.New("no devices to poll"))

	for name, factory := range map[string]func(*slog.Logger) (Collector, error){
		"device":     NewDeviceCollector,
		"plug":       NewPlugCollector,
		"strip":      NewStripCollector,
		"bulb":       NewBulbCollector,
		"lightstrip": NewLightStripCollector,
		"dimmer":     NewDimmerCollector,
		"wallswitch": NewWallSwitchCollector,
		"hub":        NewHubCollector,
	} {
		t.Run(name, func(t *testing.T) {
			c, err := factory(testLogger())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, err := collectMetrics(t, c); err == nil {
				t.Fatal("want the configuration error surfaced as a failed scrape")
			}
		})
	}
}

func TestSnapshotSurfacesTheRegistryError(t *testing.T) {
	// The real snapshot function, not the substitute the other tests install.
	// These tests run with discovery off and no addresses — nothing to poll —
	// so it must report that rather than papering over it with an empty fleet.
	if _, err := snapshot(testLogger()); err == nil {
		t.Fatal("want the configuration error surfaced")
	} else if !strings.Contains(err.Error(), "no devices to poll") {
		t.Fatalf("want the reason in the error, got %q", err)
	}
}

func TestToggleTagDescribesTheDefault(t *testing.T) {
	// The help text is how an operator knows whether a collector is on without
	// running the exporter, so both wordings have to appear, and the flag's
	// default has to agree with it.
	for _, tt := range []struct {
		isDefaultEnabled bool
		help, def        string
	}{
		{isDefaultEnabled: defaultEnabled, help: "(default: enabled)", def: "true"},
		{isDefaultEnabled: defaultDisabled, help: "(default: disabled)", def: "false"},
	} {
		tag := toggleTag("example", tt.isDefaultEnabled)
		if want, have := "collector.example", tag.Get("name"); want != have {
			t.Errorf("want the flag named %q, have %q", want, have)
		}
		if _, ok := tag.Lookup("negatable"); !ok {
			t.Errorf("want --no-collector.example to turn the collector off")
		}
		if have := tag.Get("help"); !strings.Contains(have, tt.help) {
			t.Errorf("want the help to say %q, have %q", tt.help, have)
		}
		if want, have := tt.def, tag.Get("default"); want != have {
			t.Errorf("want default %q, have %q", want, have)
		}
	}
}

func TestFlagsToggleTheCollectorState(t *testing.T) {
	t.Cleanup(SnapshotCollectorStates())
	t.Cleanup(func() {
		if _, err := parseFlags(); err != nil {
			t.Errorf("restoring defaults: %v", err)
		}
	})

	if _, err := parseFlags("--no-collector.plug", "--collector.strip"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for name, enabled := range collectorState {
		want := collectorDefaults[name]
		switch name {
		case "plug":
			want = false
		case "strip":
			want = true
		}
		if *enabled != want {
			t.Errorf("collector %q: want enabled=%t, have %t", name, want, *enabled)
		}
	}
}

func TestMarkForcedCollectorsTracksFlagsGivenOnTheCommandLine(t *testing.T) {
	// A collector named on the command line survives
	// --collector.disable-defaults, whichever way its flag points; that is what
	// MarkForcedCollectors records. Flags left at their defaults, and flags
	// that are not collector toggles, do not count.
	t.Cleanup(SnapshotCollectorStates())
	t.Cleanup(func() {
		if _, err := parseFlags(); err != nil {
			t.Errorf("restoring defaults: %v", err)
		}
	})

	clear(forcedCollectors)
	ctx, err := parseFlags("--collector.plug", "--no-collector.strip", "--kasa.timeout", "1s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	MarkForcedCollectors(ctx)

	if want := map[string]bool{"plug": true, "strip": true}; !maps.Equal(forcedCollectors, want) {
		t.Fatalf("want forced collectors %v, have %v", want, forcedCollectors)
	}
}

func TestSnapshotCollectorStatesRestoresBoth(t *testing.T) {
	// Collector state is process-wide, so a test that flips it has to be able
	// to put it back — including the forced set, which DisableDefaultCollectors
	// reads.
	restoreOuter := SnapshotCollectorStates()
	defer restoreOuter()

	*collectorState["plug"] = true
	clear(forcedCollectors)
	forcedCollectors["strip"] = true

	restore := SnapshotCollectorStates()

	*collectorState["plug"] = false
	forcedCollectors["bulb"] = true
	delete(forcedCollectors, "strip")

	restore()

	if !*collectorState["plug"] {
		t.Error("want the enabled state restored")
	}
	if !forcedCollectors["strip"] {
		t.Error("want the forced set restored")
	}
	if forcedCollectors["bulb"] {
		t.Error("want the added forced entry removed")
	}
}

func TestNewKasaCollectorSurfacesAFactoryError(t *testing.T) {
	// A collector that cannot be built must fail the handler rather than be
	// silently left out of the scrape.
	restore := SnapshotCollectorStates()
	defer restore()

	const name = "testonly-broken"
	registerCollector(name, defaultEnabled, func(*slog.Logger) (Collector, error) {
		return nil, errors.New("boom")
	})
	*collectorState[name] = true
	t.Cleanup(func() {
		delete(factories, name)
		delete(collectorState, name)
		delete(initiatedCollectors, name)
	})

	if _, err := NewKasaCollector(testLogger(), name); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNewKasaCollectorCachesInstances(t *testing.T) {
	// Collectors are built once and reused, so later scrapes do not rebuild
	// them and their descriptors.
	restore := SnapshotCollectorStates()
	defer restore()

	first, err := NewKasaCollector(testLogger(), "plug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := NewKasaCollector(testLogger(), "plug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.Collectors["plug"] != second.Collectors["plug"] {
		t.Fatal("want the same collector instance reused across scrapes")
	}
}

func TestEmitHelpersSkipAbsentValues(t *testing.T) {
	// A quantity the hardware does not measure is left out entirely; exporting
	// a zero would read as a real reading of nothing.
	desc := prometheus.NewDesc("test_metric", "help", nil, nil)
	ch := make(chan prometheus.Metric, 4)

	emitGauge(ch, desc, nil)
	emitCounter(ch, desc, nil)
	emitGauge(ch, nil, float(1))
	emitCounter(ch, nil, float(1))
	close(ch)

	if len(ch) != 0 {
		t.Fatalf("want nothing emitted, got %d metrics", len(ch))
	}
}

func TestBoolValueKeepsAbsentDistinctFromFalse(t *testing.T) {
	if boolValue(nil) != nil {
		t.Error("want an absent boolean to stay absent")
	}
	if got := boolValue(boolean(false)); got == nil || *got != 0 {
		t.Errorf("want 0 for false, got %v", got)
	}
	if got := boolValue(boolean(true)); got == nil || *got != 1 {
		t.Errorf("want 1 for true, got %v", got)
	}
}
