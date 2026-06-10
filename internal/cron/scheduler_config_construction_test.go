package cron

import (
	"testing"
	"time"
)

// TestSchedulerConfigOptionalFieldDefaults pins the documented zero-value
// fallback semantics ratified in #776 (RFC cron-config-and-structs Phase 1).
// It is a regression guard, NOT new behaviour: it asserts the post-
// NewScheduler resolved state matches the "OPTIONAL" contract written into
// the SchedulerConfig docstring. If someone changes applyDefaults() or a
// default constant without updating the docstring, this test fails.
//
// AllowNilRouter is set on every case so the router-required slog.Error
// (which is orthogonal to the field under test) stays quiet.
func TestSchedulerConfigOptionalFieldDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		cfg    SchedulerConfig
		assert func(t *testing.T, s *Scheduler)
	}{
		{
			name: "MaxJobs omitted falls back to defaultMaxJobs",
			cfg:  SchedulerConfig{AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.maxJobs != defaultMaxJobs {
					t.Errorf("maxJobs = %d, want defaultMaxJobs %d", s.maxJobs, defaultMaxJobs)
				}
			},
		},
		{
			name: "MaxJobs over hard cap is clamped",
			cfg:  SchedulerConfig{MaxJobs: maxJobsHardCap + 1000, AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.maxJobs != maxJobsHardCap {
					t.Errorf("maxJobs = %d, want maxJobsHardCap %d", s.maxJobs, maxJobsHardCap)
				}
			},
		},
		{
			name: "MaxJobsPerChat omitted falls back to DefaultMaxJobsPerChat",
			cfg:  SchedulerConfig{AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.maxJobsPerChat != DefaultMaxJobsPerChat {
					t.Errorf("maxJobsPerChat = %d, want DefaultMaxJobsPerChat %d", s.maxJobsPerChat, DefaultMaxJobsPerChat)
				}
			},
		},
		{
			name: "MaxJobsPerChat negative cannot disable the cap (R208-BL2)",
			cfg:  SchedulerConfig{MaxJobsPerChat: -5, AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.maxJobsPerChat != DefaultMaxJobsPerChat {
					t.Errorf("maxJobsPerChat = %d for negative input, want DefaultMaxJobsPerChat %d (cap must not be disablable)", s.maxJobsPerChat, DefaultMaxJobsPerChat)
				}
			},
		},
		{
			name: "MaxJobsPerChat positive override is honoured",
			cfg:  SchedulerConfig{MaxJobsPerChat: 3, AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.maxJobsPerChat != 3 {
					t.Errorf("maxJobsPerChat = %d, want override 3", s.maxJobsPerChat)
				}
			},
		},
		{
			name: "ExecTimeout omitted falls back to defaultExecTimeout",
			cfg:  SchedulerConfig{AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.execTimeout != defaultExecTimeout {
					t.Errorf("execTimeout = %v, want defaultExecTimeout %v", s.execTimeout, defaultExecTimeout)
				}
			},
		},
		{
			name: "Location omitted falls back to time.Local",
			cfg:  SchedulerConfig{AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.location != time.Local {
					t.Errorf("location = %v, want time.Local", s.location)
				}
			},
		},
		{
			name: "Location explicit value is preserved",
			cfg:  SchedulerConfig{Location: time.UTC, AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.location != time.UTC {
					t.Errorf("location = %v, want time.UTC", s.location)
				}
			},
		},
		{
			// HONESTY NOTE: SlowThreshold has NO field-level default. Unlike
			// MaxJobs/ExecTimeout/Location (resolved in applyDefaults), the
			// raw zero is stored on s.slowThreshold and the defaultCron-
			// SlowThreshold fallback is applied lazily at the read site in
			// scheduler_run.go. So the docstring's "<=0 → defaultCron-
			// SlowThreshold (read lazily at the callsite)" is the precise
			// contract: the FIELD stays 0, not the default. Pin both halves.
			name: "SlowThreshold omitted leaves the field zero (lazy callsite fallback)",
			cfg:  SchedulerConfig{AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.slowThreshold != 0 {
					t.Errorf("slowThreshold = %v, want 0 (fallback is applied lazily at the callsite, not on the field)", s.slowThreshold)
				}
			},
		},
		{
			name: "SlowThreshold positive override is stored on the field",
			cfg:  SchedulerConfig{SlowThreshold: 90 * time.Second, AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.slowThreshold != 90*time.Second {
					t.Errorf("slowThreshold = %v, want 90s override", s.slowThreshold)
				}
			},
		},
		{
			name: "AllowedRoot omitted means no root constraint",
			cfg:  SchedulerConfig{AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.allowedRoot != "" {
					t.Errorf("allowedRoot = %q, want empty (no constraint)", s.allowedRoot)
				}
				if s.allowedRootResolved != "" {
					t.Errorf("allowedRootResolved = %q, want empty when AllowedRoot omitted", s.allowedRootResolved)
				}
			},
		},
		{
			name: "AllowedRoot with NUL byte is cleared (no constraint)",
			cfg:  SchedulerConfig{AllowedRoot: "/tmp/\x00evil", AllowNilRouter: true},
			assert: func(t *testing.T, s *Scheduler) {
				if s.allowedRoot != "" {
					t.Errorf("allowedRoot = %q for NUL-bearing input, want cleared to empty", s.allowedRoot)
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := NewScheduler(tt.cfg, SchedulerDeps{})
			tt.assert(t, s)
		})
	}
}

// TestSchedulerConfigApplyDefaultsIdempotent pins the documented idempotency
// of applyDefaults — a second call on an already-defaulted config is a no-op.
// This backs the "single source of truth / idempotent" claim in the
// SchedulerConfig docstring.
func TestSchedulerConfigApplyDefaultsIdempotent(t *testing.T) {
	t.Parallel()

	cfg := SchedulerConfig{}
	cfg.applyDefaults()
	first := cfg
	cfg.applyDefaults()

	if cfg.MaxJobs != first.MaxJobs ||
		cfg.MaxJobsPerChat != first.MaxJobsPerChat ||
		cfg.ExecTimeout != first.ExecTimeout ||
		cfg.Location != first.Location {
		t.Errorf("applyDefaults not idempotent: first=%+v second=%+v", first, cfg)
	}

	if cfg.MaxJobs != defaultMaxJobs {
		t.Errorf("MaxJobs = %d, want %d", cfg.MaxJobs, defaultMaxJobs)
	}
	if cfg.MaxJobsPerChat != DefaultMaxJobsPerChat {
		t.Errorf("MaxJobsPerChat = %d, want %d", cfg.MaxJobsPerChat, DefaultMaxJobsPerChat)
	}
	if cfg.ExecTimeout != defaultExecTimeout {
		t.Errorf("ExecTimeout = %v, want %v", cfg.ExecTimeout, defaultExecTimeout)
	}
	if cfg.Location != time.Local {
		t.Errorf("Location = %v, want time.Local", cfg.Location)
	}
}
