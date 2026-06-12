package cli

import (
	"sync/atomic"
	"testing"
)

// TestWrapper_EffectiveVersion_PrefersLive pins R20260612-global-version:
// EffectiveVersion returns the live observed version once a spawned process
// has reported one, and falls back to the spawn-time CLIVersion before that.
func TestWrapper_EffectiveVersion_PrefersLive(t *testing.T) {
	w := &Wrapper{CLIVersion: "2.1.100"}

	if got := w.EffectiveVersion(); got != "2.1.100" {
		t.Fatalf("EffectiveVersion before observe = %q, want spawn-time 2.1.100", got)
	}

	w.ObserveLiveVersion("2.1.174")
	if got := w.EffectiveVersion(); got != "2.1.174" {
		t.Fatalf("EffectiveVersion after observe = %q, want live 2.1.174", got)
	}
}

// TestWrapper_ObserveLiveVersion_IgnoresEmpty guards that an empty observation
// (defensive; the Process side never sends "") cannot blank a previously
// observed live version nor the spawn-time fallback.
func TestWrapper_ObserveLiveVersion_IgnoresEmpty(t *testing.T) {
	w := &Wrapper{CLIVersion: "2.1.100"}
	w.ObserveLiveVersion("2.1.174")
	w.ObserveLiveVersion("")
	if got := w.EffectiveVersion(); got != "2.1.174" {
		t.Fatalf("EffectiveVersion = %q after empty observe, want 2.1.174 retained", got)
	}
}

// TestProcess_SetLiveVersion_FiresCallbackOnChange pins that setLiveVersion
// invokes onLiveVersion exactly once per distinct version: the duplicate
// captures (readLoop + Send both hook the init frame) must not double-fire,
// but a genuinely new version (e.g. reconnect onto an upgraded binary) must.
func TestProcess_SetLiveVersion_FiresCallbackOnChange(t *testing.T) {
	p := &Process{}
	var calls atomic.Int32
	var last atomic.Pointer[string]
	p.SetOnLiveVersion(func(v string) {
		calls.Add(1)
		s := v
		last.Store(&s)
	})

	p.setLiveVersion("2.1.174") // first observation → fire
	p.setLiveVersion("2.1.174") // duplicate (Send re-hook) → no fire
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback fired %d times for duplicate version, want 1", got)
	}

	p.setLiveVersion("2.1.180") // changed (upgrade) → fire
	if got := calls.Load(); got != 2 {
		t.Fatalf("callback fired %d times after version change, want 2", got)
	}
	if v := last.Load(); v == nil || *v != "2.1.180" {
		t.Fatalf("callback last value = %v, want 2.1.180", v)
	}
}

// TestProcess_SetLiveVersion_NoCallbackWiredIsSafe guards the legacy
// &Process{} path: setLiveVersion with no callback assigned must not panic.
func TestProcess_SetLiveVersion_NoCallbackWiredIsSafe(t *testing.T) {
	p := &Process{}
	p.setLiveVersion("2.1.174")
	if got := p.LiveVersion(); got != "2.1.174" {
		t.Fatalf("LiveVersion = %q, want 2.1.174", got)
	}
}
