package session

import "testing"

// TestIsAutoChainSkippedKey locks the namespace policy of the
// auto-chain skip set — see #1212 + isAutoChainSkippedKey godoc for
// rationale on the project-key carve-out.
//
// If a future namespace lands and forgets to update isAutoChainSkippedKey,
// this test will not fail (Go can't enumerate "unknown future prefixes")
// — but the existing entries are pinned, so a regression that drops
// cron/sys/scratch from the skip set or accidentally adds project to it
// is caught at compile time.
func TestIsAutoChainSkippedKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"cron is skipped", "cron:job-7", true},
		{"sys is skipped", "sys:auto-titler", true},
		{"scratch is skipped", "scratch:abc123", true},
		// Project planner is INTENTIONALLY not skipped — backfill
		// must be allowed to retroactively populate prev_session_ids.
		{"project planner is NOT skipped", "project:foo:planner", false},
		// Standard IM key shape: not in any reserved namespace.
		{"feishu im key not skipped", "feishu:p2p:user-7:default", false},
		{"empty key not skipped", "", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isAutoChainSkippedKey(tc.key); got != tc.want {
				t.Fatalf("isAutoChainSkippedKey(%q) = %v, want %v",
					tc.key, got, tc.want)
			}
		})
	}
}
