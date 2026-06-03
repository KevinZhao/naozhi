package shim

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
)

// countingHandler is a minimal slog.Handler that counts emitted records.
type countingHandler struct {
	n atomic.Int64
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h *countingHandler) Handle(context.Context, slog.Record) error { h.n.Add(1); return nil }
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h *countingHandler) WithGroup(string) slog.Handler             { return h }

// TestFilterShimEnv_OversizeWarningCap pins [R20260603-SEC-5]: when many
// oversized env entries are present, the warning log is capped at
// maxShimEnvOversizeWarnings (5) rather than emitting exactly one (the old
// sync.Once behavior that let a benign oversized entry mask later
// attacker-injected ones). All oversized entries are still dropped.
//
// Not parallel: it swaps the process-global slog default and resets the
// process-global oversize counter, both shared with other tests.
func TestFilterShimEnv_OversizeWarningCap(t *testing.T) {
	// Reset the shared counter so this test sees a clean budget regardless of
	// other tests that may have incremented it earlier in the run.
	filterShimEnvOversizeWarnings.Store(0)

	prev := slog.Default()
	h := &countingHandler{}
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	big := strings.Repeat("x", maxShimEnvEntryBytes+1)
	const oversizedCount = 12
	input := make([]string, 0, oversizedCount+1)
	input = append(input, "HOME=/home/user") // one benign, allowed entry
	for i := 0; i < oversizedCount; i++ {
		input = append(input, "BIG_VAR="+big) // oversized — must be dropped
	}

	got := filterShimEnv(input)

	// Only the benign HOME entry survives; every oversized entry is dropped.
	if len(got) != 1 || got[0] != "HOME=/home/user" {
		t.Fatalf("expected only HOME to survive, got %v", got)
	}

	if n := h.n.Load(); n != maxShimEnvOversizeWarnings {
		t.Fatalf("expected %d oversize warnings (capped), got %d", maxShimEnvOversizeWarnings, n)
	}
}
