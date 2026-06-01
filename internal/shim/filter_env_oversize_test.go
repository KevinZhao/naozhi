package shim

import (
	"strings"
	"testing"
)

// TestFilterShimEnv_OversizedEntryRejected pins [R112714-ARCH-3]: entries
// exceeding maxShimEnvEntryBytes must be rejected (not forwarded to the
// shim subprocess), and the key prefix must be extractable for logging.
func TestFilterShimEnv_OversizedEntryRejected(t *testing.T) {
	t.Parallel()
	bigVal := strings.Repeat("x", maxShimEnvEntryBytes+1)
	input := []string{
		"HOME=/home/user",           // allowed, normal
		"PYTHONPATH=" + bigVal,      // oversized — must be rejected
		"ANTHROPIC_API_KEY=sk-test", // allowed, normal
		"BIG_SECRET=" + bigVal,      // oversized — must be rejected
	}
	got := filterShimEnv(input)
	// Only the two normal entries must survive.
	if len(got) != 2 {
		t.Fatalf("expected 2 entries (oversized rejected), got %d: %v", len(got), got)
	}
	for _, kv := range got {
		if len(kv) > maxShimEnvEntryBytes {
			t.Errorf("oversized entry leaked through filterShimEnv: len=%d key=%s", len(kv), kvKeyPrefix(kv))
		}
	}
}

// TestKvKeyPrefix covers edge cases of the helper that extracts the key name
// for log output (ensuring no value data leaks).
func TestKvKeyPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  string
	}{
		{"HOME=/home/user", "HOME"},
		{"ANTHROPIC_API_KEY=sk-secret-value", "ANTHROPIC_API_KEY"},
		{"NO_EQUALS", "NO_EQUALS"},
		{"=value_only", ""},
		{strings.Repeat("K", 100) + "=val", strings.Repeat("K", 64)}, // truncated at 64
	}
	for _, tc := range cases {
		got := kvKeyPrefix(tc.input)
		if got != tc.want {
			t.Errorf("kvKeyPrefix(%q) = %q, want %q", tc.input, got, tc.want)
		}
		// Verify the value portion never appears in the output.
		if strings.Contains(got, "secret") || strings.Contains(got, "value") || strings.Contains(got, "val") {
			t.Errorf("kvKeyPrefix(%q) leaked value portion: %q", tc.input, got)
		}
	}
}
