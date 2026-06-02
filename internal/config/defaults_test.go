package config

import (
	"testing"
	"time"
)

// TestApplyDefaults_UsesCentralizedConstants verifies that an empty Config gets
// the centralized default values written by applyDefaults, and that the string
// defaults round-trip to the same time.Duration the parse-fallbacks use. This
// is the regression guard for R247-ARCH-8 / #630: the string default and the
// parse fallback are now derived from one constant and must never diverge.
func TestApplyDefaults_UsesCentralizedConstants(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	applyDefaults(cfg)

	if cfg.Server.Addr != defaultServerAddr {
		t.Errorf("Server.Addr = %q, want %q", cfg.Server.Addr, defaultServerAddr)
	}
	if cfg.Log.Level != defaultLogLevel {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, defaultLogLevel)
	}
	if cfg.Session.Queue.Mode != defaultQueueMode {
		t.Errorf("Queue.Mode = %q, want %q", cfg.Session.Queue.Mode, defaultQueueMode)
	}
	if cfg.Session.Queue.MaxDepth == nil || *cfg.Session.Queue.MaxDepth != defaultQueueMaxDepth {
		t.Errorf("Queue.MaxDepth = %v, want %d", cfg.Session.Queue.MaxDepth, defaultQueueMaxDepth)
	}

	// String defaults must parse back to the same Duration the fallbacks use.
	cases := []struct {
		name string
		got  string
		want time.Duration
	}{
		{"session.ttl", cfg.Session.TTL, defaultSessionTTL},
		{"session.prune_ttl", cfg.Session.PruneTTL, defaultSessionPruneTTL},
		{"queue.collect_delay", cfg.Session.Queue.CollectDelay, defaultQueueCollectDelay},
	}
	for _, c := range cases {
		d, err := time.ParseDuration(c.got)
		if err != nil {
			t.Errorf("%s: ParseDuration(%q) error: %v", c.name, c.got, err)
			continue
		}
		if d != c.want {
			t.Errorf("%s: default %q parses to %v, want %v", c.name, c.got, d, c.want)
		}
	}
}

// TestParseDurations_FallbacksMatchDefaults confirms that when the string
// fields are left empty (the explicit-disable / empty path), parseDurations
// falls back to the centralized constants, matching what applyDefaults would
// have written.
func TestParseDurations_FallbacksMatchDefaults(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	if err := parseDurations(cfg); err != nil {
		t.Fatalf("parseDurations: %v", err)
	}
	if cfg.cachedTTL != defaultSessionTTL {
		t.Errorf("cachedTTL = %v, want %v", cfg.cachedTTL, defaultSessionTTL)
	}
	if cfg.cachedPruneTTL != defaultSessionPruneTTL {
		t.Errorf("cachedPruneTTL = %v, want %v", cfg.cachedPruneTTL, defaultSessionPruneTTL)
	}
	if cfg.cachedExecTimeout != defaultCronExecTimeout {
		t.Errorf("cachedExecTimeout = %v, want %v", cfg.cachedExecTimeout, defaultCronExecTimeout)
	}
	if cfg.cachedJitterMax != defaultCronJitterMax {
		t.Errorf("cachedJitterMax = %v, want %v", cfg.cachedJitterMax, defaultCronJitterMax)
	}
}
