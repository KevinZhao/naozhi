package shim

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
)

// TestFilterShimEnv_EndpointSSRFGuard pins R20260602-SEC-1 (#1576, shim
// sibling): endpoint base-URL env vars pointing at a plain-http non-loopback
// host (attacker / IMDS) must be dropped before reaching the CLI subprocess,
// while https endpoints and loopback http mocks pass through. Non-endpoint
// allowlisted vars are unaffected.
func TestFilterShimEnv_EndpointSSRFGuard(t *testing.T) {
	t.Parallel()
	input := []string{
		"HOME=/home/user",                              // allowed, non-URL
		"ANTHROPIC_API_KEY=sk-test",                    // allowed, non-URL
		"ANTHROPIC_BASE_URL=http://169.254.169.254",    // IMDS plain http — drop
		"ANTHROPIC_BEDROCK_BASE_URL=http://evil.test",  // attacker plain http — drop
		"AWS_ENDPOINT_URL=https://gw.corp.example.com", // https — keep
		"AWS_BEDROCK_ENDPOINT=http://127.0.0.1:8080",   // loopback http — keep
	}
	got := filterShimEnv(input)
	gotSet := map[string]bool{}
	for _, kv := range got {
		gotSet[kv] = true
	}

	mustKeep := []string{
		"HOME=/home/user",
		"ANTHROPIC_API_KEY=sk-test",
		"AWS_ENDPOINT_URL=https://gw.corp.example.com",
		"AWS_BEDROCK_ENDPOINT=http://127.0.0.1:8080",
	}
	for _, kv := range mustKeep {
		if !gotSet[kv] {
			t.Errorf("expected %q to be kept, but it was dropped; got=%v", kv, got)
		}
	}
	mustDrop := []string{
		"ANTHROPIC_BASE_URL=http://169.254.169.254",
		"ANTHROPIC_BEDROCK_BASE_URL=http://evil.test",
	}
	for _, kv := range mustDrop {
		if gotSet[kv] {
			t.Errorf("expected %q to be dropped (SSRF guard), but it leaked through", kv)
		}
	}
}

func TestValidateShimEndpointURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v       string
		wantErr bool
	}{
		{"https://api.anthropic.com", false},
		{"http://127.0.0.1:9000", false},
		{"http://[::1]:9000", false},
		{"http://localhost", false},
		{"http://169.254.169.254", true},
		{"http://attacker.test", true},
		{"ftp://host", true},
		// R20260603150052-SEC-7 (#1713): https:// to internal/IMDS IPs must be
		// rejected even though the scheme is https.
		{"https://169.254.169.254/latest/meta-data", true}, // EC2 IMDS
		{"https://10.0.0.5", true},                         // RFC1918 private
		{"https://192.168.1.1", true},                      // RFC1918 private
		{"https://[fd00::1]", true},                        // ULA private
		{"https://127.0.0.1:8443", false},                  // loopback https mock — keep
		{"https://gw.corp.example.com", false},             // hostname https — keep
	}
	for _, tc := range cases {
		err := validateShimEndpointURL(tc.v)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateShimEndpointURL(%q) err=%v, wantErr=%v", tc.v, err, tc.wantErr)
		}
	}
}

// TestShimEndpointEnvDropped_EachRejectionLogged verifies R164029-GO-1: every
// unsafe endpoint variable is logged individually. Previously a sync.Once
// caused the second (and further) rejections to be silently swallowed, leaving
// operators unable to discover that multiple env vars were contaminated.
func TestShimEndpointEnvDropped_EachRejectionLogged(t *testing.T) {
	// Install a temporary slog handler that counts Warn records keyed by the
	// "key" attribute value. We replace the default logger for the duration
	// of this test and restore it after.
	var warnCount atomic.Int64
	keys := make(map[string]int)
	var mu sync.Mutex // protect keys map

	handler := &countingWarnHandler{
		onWarn: func(key string) {
			mu.Lock()
			keys[key]++
			mu.Unlock()
			warnCount.Add(1)
		},
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Two unsafe entries; both must be dropped and each must emit its own Warn.
	unsafe := []string{
		"ANTHROPIC_BASE_URL=http://169.254.169.254",
		"ANTHROPIC_BEDROCK_BASE_URL=http://evil.test",
	}
	for _, kv := range unsafe {
		if !shimEndpointEnvDropped(kv) {
			t.Errorf("shimEndpointEnvDropped(%q) = false, want true", kv)
		}
	}

	if got := warnCount.Load(); got != 2 {
		t.Errorf("expected 2 Warn log calls (one per rejection), got %d", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_BASE_URL"} {
		if keys[k] != 1 {
			t.Errorf("expected exactly 1 Warn for key %q, got %d", k, keys[k])
		}
	}
}

// countingWarnHandler is a minimal slog.Handler that invokes onWarn for every
// Warn-level record, passing the value of the "key" attribute.
type countingWarnHandler struct {
	onWarn func(key string)
}

func (h *countingWarnHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *countingWarnHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		var k string
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "key" {
				k = a.Value.String()
				return false
			}
			return true
		})
		h.onWarn(k)
	}
	return nil
}

func (h *countingWarnHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *countingWarnHandler) WithGroup(_ string) slog.Handler      { return h }
