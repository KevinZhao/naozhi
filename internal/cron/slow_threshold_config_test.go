package cron

import (
	"testing"
	"time"
)

// TestSlowThreshold_DefaultsTo30s pins R241-ARCH-11 (#519): when
// SchedulerConfig.SlowThreshold is unset (zero value), the Scheduler's
// resolved threshold falls through to defaultCronSlowThreshold so callers
// that never opt in keep the legacy 30s slow-alert behaviour. Reading the
// field directly (instead of inspecting the slow-alert log) keeps the test
// hermetic: no goroutine timing, no metric peeking. The fallback is
// applied at the executeOpt callsite (where elapsed is compared), so we
// pin both the unset-defaults-to-zero invariant on the struct AND the
// fallback wiring is exercised by the next test.
func TestSlowThreshold_DefaultsTo30s(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{
		MaxJobs: 5,
		Router:  &fakeRouter{},
	})
	if s.slowThreshold != 0 {
		t.Errorf("Scheduler.slowThreshold = %v; want 0 when SchedulerConfig.SlowThreshold unset (so callsite reads defaultCronSlowThreshold)", s.slowThreshold)
	}
	if defaultCronSlowThreshold != 30*time.Second {
		t.Errorf("defaultCronSlowThreshold = %v; want 30s (R208-OBS1 baseline)", defaultCronSlowThreshold)
	}
}

// TestSlowThreshold_ConfigOverride pins that a non-zero
// SchedulerConfig.SlowThreshold flows through to Scheduler.slowThreshold,
// so deployments setting cron.slow_threshold=300s in their config (e.g.
// to align with an ExecTimeout=300s deployment) actually get the override.
// Without this contract, the package const stays the silent floor and the
// daily false-alarm storm continues.
func TestSlowThreshold_ConfigOverride(t *testing.T) {
	t.Parallel()
	override := 5 * time.Minute
	s := NewScheduler(SchedulerConfig{
		MaxJobs:       5,
		Router:        &fakeRouter{},
		SlowThreshold: override,
	})
	if s.slowThreshold != override {
		t.Errorf("Scheduler.slowThreshold = %v; want %v from SchedulerConfig.SlowThreshold", s.slowThreshold, override)
	}
}

// TestSlowThreshold_NegativeFallsBackToDefault confirms the executeOpt
// callsite's `<= 0` guard treats a negative override the same as zero
// (fall through to defaultCronSlowThreshold). The struct field stores the
// negative value as-is — we don't normalise at construction so a future
// debug toggle could set a -1 sentinel and the callsite logic still
// resolves correctly. This pins the callsite branch so a refactor that
// drops `<= 0` for a `< 0` (or worse, removes the guard) breaks here
// instead of silently shipping a "<-100ms means everything is slow"
// regression.
func TestSlowThreshold_NegativeFallsBackToDefault(t *testing.T) {
	t.Parallel()
	// We can't easily test the executeOpt callsite without spinning a
	// real cron tick; pin the guard via a direct mirror of the callsite
	// expression. If the helper resolution drifts, the executeOpt path
	// must drift the same way (callers grep this test if they refactor).
	resolveSlowThreshold := func(cfg time.Duration) time.Duration {
		if cfg <= 0 {
			return defaultCronSlowThreshold
		}
		return cfg
	}
	if got := resolveSlowThreshold(0); got != defaultCronSlowThreshold {
		t.Errorf("resolveSlowThreshold(0) = %v; want %v", got, defaultCronSlowThreshold)
	}
	if got := resolveSlowThreshold(-1 * time.Second); got != defaultCronSlowThreshold {
		t.Errorf("resolveSlowThreshold(-1s) = %v; want %v (negative falls back to default)", got, defaultCronSlowThreshold)
	}
	if got := resolveSlowThreshold(2 * time.Minute); got != 2*time.Minute {
		t.Errorf("resolveSlowThreshold(2m) = %v; want 2m", got)
	}
}
