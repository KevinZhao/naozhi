package cli

import (
	"context"
	"expvar"
	"log/slog"
	"sync"
	"testing"
)

// TestSpawnDiagsFor_DeniedFlags reproduces #2412 (`--effort high` smuggled in
// via config args) and #2493 (`--append-system-prompt`): the argv denylist
// strips them, and the diag names each stripped flag once.
func TestSpawnDiagsFor_DeniedFlags(t *testing.T) {
	t.Parallel()
	opts := SpawnOptions{ExtraArgs: []string{
		"--effort", "high",
		"--append-system-prompt=be brief",
		"--effort=max", // repeat of an already-reported flag
		"--debug",      // legitimate operator flag, untouched
	}}
	diags := SpawnDiagsFor(opts, Caps{EffortTier: true})
	want := map[string]bool{"--effort": false, "--append-system-prompt": false}
	for _, d := range diags {
		if d.Layer != "argv-denylist" || d.Action != "dropped" {
			t.Errorf("diag %+v: want layer=argv-denylist action=dropped", d)
		}
		if seen, ok := want[d.Key]; !ok || seen {
			t.Errorf("unexpected or duplicate diag key %q", d.Key)
		} else {
			want[d.Key] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing diag for stripped flag %q", k)
		}
	}
	// Parity: every diagnosed flag really is stripped from the argv.
	kept := capExtraArgsBytes(opts.ExtraArgs)
	for _, a := range kept {
		if isDeniedFlag(a) {
			t.Errorf("denied flag %q survived capExtraArgsBytes", a)
		}
	}
}

// TestSpawnDiagsFor_EffortCapsGate covers the capability gate: a configured
// tier on a backend without EffortTier is reported as ignored.
func TestSpawnDiagsFor_EffortCapsGate(t *testing.T) {
	t.Parallel()
	diags := SpawnDiagsFor(SpawnOptions{Effort: "high"}, Caps{EffortTier: false})
	if len(diags) != 1 || diags[0].Layer != "caps" || diags[0].Key != "effort" || diags[0].Action != "ignored" {
		t.Fatalf("diags = %+v, want one caps/effort/ignored", diags)
	}
	if got := SpawnDiagsFor(SpawnOptions{Effort: "high"}, Caps{EffortTier: true}); len(got) != 0 {
		t.Fatalf("EffortTier backend must produce no caps diag, got %+v", got)
	}
}

// TestSpawnDiagsFor_ByteCap: an over-cap ExtraArgs slice reports a single
// whole-slice drop instead of per-flag noise.
func TestSpawnDiagsFor_ByteCap(t *testing.T) {
	t.Parallel()
	huge := make([]byte, maxExtraArgsBytes+1)
	diags := SpawnDiagsFor(SpawnOptions{ExtraArgs: []string{string(huge), "--effort=high"}}, Caps{})
	if len(diags) != 1 || diags[0].Key != "args" || diags[0].Action != "dropped" {
		t.Fatalf("diags = %+v, want one args/dropped", diags)
	}
}

// levelCountingHandler counts records per level.
type levelCountingHandler struct {
	mu     sync.Mutex
	counts map[slog.Level]int
}

func (h *levelCountingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *levelCountingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.counts[r.Level]++
	h.mu.Unlock()
	return nil
}
func (h *levelCountingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *levelCountingHandler) WithGroup(_ string) slog.Handler      { return h }

// TestEmitSpawnDiags_MetricsAndDedup asserts the acceptance contract of
// #2532: a denied-flag emission bumps naozhi_spawn_diag_total for its
// (layer, action) tuple by exactly 1, the first emission logs at Warn, and a
// repeat for the same scope+layer+key (the 30s reconcile heartbeat) logs at
// Debug without moving the counter.
func TestEmitSpawnDiags_MetricsAndDedup(t *testing.T) {
	h := &levelCountingHandler{counts: map[slog.Level]int{}}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	counter, ok := expvar.Get("naozhi_spawn_diag_total").(*expvar.Map)
	if !ok {
		t.Fatal("naozhi_spawn_diag_total not registered as a labeled map")
	}
	read := func() int64 {
		if v, ok := counter.Get("argv-denylist|dropped").(*expvar.Int); ok {
			return v.Value()
		}
		return 0
	}

	diags := []SpawnDiag{{Layer: "argv-denylist", Key: "--effort", Action: "dropped", Reason: "test"}}
	before := read()

	EmitSpawnDiags("test-scope-2532", diags)
	if got := read() - before; got != 1 {
		t.Fatalf("first emission moved counter by %d, want 1", got)
	}
	if h.counts[slog.LevelWarn] != 1 || h.counts[slog.LevelDebug] != 0 {
		t.Fatalf("first emission logged warn=%d debug=%d, want 1/0", h.counts[slog.LevelWarn], h.counts[slog.LevelDebug])
	}

	EmitSpawnDiags("test-scope-2532", diags) // heartbeat repeat
	if got := read() - before; got != 1 {
		t.Fatalf("repeat emission moved counter to +%d, want still +1", got)
	}
	if h.counts[slog.LevelWarn] != 1 || h.counts[slog.LevelDebug] != 1 {
		t.Fatalf("repeat logged warn=%d debug=%d, want 1/1", h.counts[slog.LevelWarn], h.counts[slog.LevelDebug])
	}

	// A different scope (another session) warns again.
	EmitSpawnDiags("test-scope-2532-b", diags)
	if h.counts[slog.LevelWarn] != 2 {
		t.Fatalf("new scope logged warn=%d, want 2", h.counts[slog.LevelWarn])
	}
}
