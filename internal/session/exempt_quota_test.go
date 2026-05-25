package session

import "testing"

// TestExemptKindClassification pins R242-ARCH-2: each exempt prefix
// resolves to its own namespace string and non-exempt keys return "".
// Sub-quota dispatch in spawnSession depends on this mapping; a
// classification regression would silently route a stub into the wrong
// quota.
func TestExemptKindClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key  string
		want string
	}{
		{"cron:abc123", "cron"},
		{"cron:" + "x", "cron"},
		{"project:my-proj:planner", "project"},
		{"sys:auto-titler", "sys"},
		{"feishu:direct:alice:general", ""},
		{"scratch:feishu:direct:alice:general", ""},
		{"", ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()
			if got := exemptKind(c.key); got != c.want {
				t.Errorf("exemptKind(%q) = %q, want %q", c.key, got, c.want)
			}
		})
	}
}

// TestExemptCapFor pins the sub-quota lookup. Adding a new exempt
// namespace should also extend exemptCapFor; missing wiring falls
// back to the global maxExemptSessions ceiling so an unconfigured
// namespace still has a defined limit.
func TestExemptCapFor(t *testing.T) {
	t.Parallel()
	if got := exemptCapFor("cron"); got != maxCronExempt {
		t.Errorf("exemptCapFor(cron) = %d, want %d", got, maxCronExempt)
	}
	if got := exemptCapFor("project"); got != maxProjectExempt {
		t.Errorf("exemptCapFor(project) = %d, want %d", got, maxProjectExempt)
	}
	if got := exemptCapFor("sys"); got != maxSysExempt {
		t.Errorf("exemptCapFor(sys) = %d, want %d", got, maxSysExempt)
	}
	if got := exemptCapFor("unknown"); got != maxExemptSessions {
		t.Errorf("exemptCapFor(unknown) = %d, want %d (global fallback)", got, maxExemptSessions)
	}
}

// TestExemptSubQuotasFitGlobalCeiling guards the design invariant
// "sum of sub-quotas ≤ maxExemptSessions" so the global ceiling stays
// the relief valve, never the primary trigger. R242-ARCH-2.
func TestExemptSubQuotasFitGlobalCeiling(t *testing.T) {
	t.Parallel()
	sum := maxCronExempt + maxProjectExempt + maxSysExempt
	if sum > maxExemptSessions {
		t.Fatalf("sum of sub-quotas %d exceeds maxExemptSessions %d", sum, maxExemptSessions)
	}
}
