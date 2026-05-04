package session

import "testing"

// TestReservedNamespacePrefixes locks the canonical reserved prefix set.
// Adding a new namespace (e.g. future "gemini:") requires updating this
// test in lockstep with reservedKeyPrefixes, DESIGN.md, and any filter
// that previously listed the prefixes inline.
func TestReservedNamespacePrefixes(t *testing.T) {
	t.Parallel()
	// Sanity: canonical constants carry their trailing colon so substring
	// checks cannot accidentally match "cronographer:" or "projectile:".
	for _, p := range []string{CronKeyPrefix, ProjectKeyPrefix, ScratchKeyPrefix} {
		if p == "" {
			t.Errorf("empty reserved prefix in set")
		}
		if p[len(p)-1] != ':' {
			t.Errorf("reserved prefix %q missing trailing colon", p)
		}
	}

	// The reservedKeyPrefixes slice must include every exported prefix
	// constant; drifting here is exactly the foot-gun R176-ARCH-M1 was
	// meant to prevent.
	wantSet := map[string]bool{
		CronKeyPrefix:    true,
		ProjectKeyPrefix: true,
		ScratchKeyPrefix: true,
	}
	for _, p := range reservedKeyPrefixes {
		if !wantSet[p] {
			t.Errorf("reservedKeyPrefixes contains unexpected entry %q", p)
		}
		delete(wantSet, p)
	}
	for missing := range wantSet {
		t.Errorf("reservedKeyPrefixes missing expected entry %q", missing)
	}
}

func TestIsReservedNamespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"empty", "", false},
		{"standard IM key", "feishu:group:chat-123:general", false},
		{"standard IM direct", "slack:direct:U123:general", false},
		{"cron", "cron:job-123", true},
		{"cron bare prefix", "cron:", true},
		{"project planner", "project:myrepo:planner", true},
		{"project bare prefix", "project:", true},
		{"scratch", "scratch:abc:general:general", true},
		{"scratch bare prefix", "scratch:", true},
		// Substring collisions must NOT be classified as reserved: keeping
		// trailing-colon tokens in the prefix set prevents this, but
		// testing it explicitly locks the contract so future refactors
		// that drop the colon (e.g. an over-eager "clean up constants"
		// pass) will surface the regression.
		{"cronographer false positive", "cronographer:direct:1:x", false},
		{"projectile false positive", "projectile:direct:1:x", false},
		{"scratchpad false positive", "scratchpad:direct:1:x", false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsReservedNamespace(tt.key); got != tt.want {
				t.Errorf("IsReservedNamespace(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestIsCronKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"empty", "", false},
		{"cron job", "cron:job-abc", true},
		{"cron bare prefix", "cron:", true},
		{"project", "project:myrepo:planner", false},
		{"feishu", "feishu:group:chat:general", false},
		{"cronographer false positive", "cronographer:x", false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsCronKey(tt.key); got != tt.want {
				t.Errorf("IsCronKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// TestExemptKeyPrefixesUsesConstants locks the invariant that
// router.go's exemptKeyPrefixes references the canonical constants,
// not bare string literals that could drift. The concrete values live
// on the constants; this test fails loudly if exemptKeyPrefixes is
// ever re-grown with literal strings.
func TestExemptKeyPrefixesUsesConstants(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		CronKeyPrefix:    true,
		ProjectKeyPrefix: true,
	}
	for _, p := range exemptKeyPrefixes {
		if !want[p] {
			t.Errorf("exemptKeyPrefixes has unexpected entry %q; "+
				"exempt-namespace policy must compose from the "+
				"canonical reserved-prefix constants in key.go", p)
		}
		delete(want, p)
	}
	for missing := range want {
		t.Errorf("exemptKeyPrefixes missing expected constant %q", missing)
	}
}

// TestIsExemptKey exercises the router-private predicate that drives TTL
// exemption. Kept in the key_test.go file because the policy set it
// derives from lives here.
func TestIsExemptKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"cron", "cron:foo", true},
		{"project", "project:bar:planner", true},
		{"scratch NOT exempt", "scratch:abc:general:general", false},
		{"IM NOT exempt", "feishu:group:chat:general", false},
		{"empty NOT exempt", "", false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isExemptKey(tt.key); got != tt.want {
				t.Errorf("isExemptKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}
