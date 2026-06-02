package session

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// TestProjectStableKey_Deterministic: same inputs → same key, every call.
func TestProjectStableKey_Deterministic(t *testing.T) {
	t.Parallel()
	k1 := ProjectStableKey("/home/ec2-user/workspace/naozhi", "general")
	k2 := ProjectStableKey("/home/ec2-user/workspace/naozhi", "general")
	if k1 != k2 {
		t.Fatalf("non-deterministic: %q != %q", k1, k2)
	}
}

// TestProjectStableKey_CleanNormalizes: trailing slash / "." / redundant
// separators all clean to the same path → same key.
func TestProjectStableKey_CleanNormalizes(t *testing.T) {
	t.Parallel()
	base := ProjectStableKey("/a/b", "general")
	for _, variant := range []string{"/a/b/", "/a/b/.", "/a//b", "/a/c/../b"} {
		if got := ProjectStableKey(variant, "general"); got != base {
			t.Errorf("ProjectStableKey(%q) = %q, want %q (clean-equal to /a/b)", variant, got, base)
		}
	}
}

// TestProjectStableKey_DistinctPaths: the basename-collision fix. Two
// different absolute paths with the same basename must yield different keys.
func TestProjectStableKey_DistinctPaths(t *testing.T) {
	t.Parallel()
	ka := ProjectStableKey("/x/foo", "general")
	kb := ProjectStableKey("/y/foo", "general")
	if ka == kb {
		t.Fatalf("basename collision not fixed: /x/foo and /y/foo both → %q", ka)
	}
}

// TestProjectStableKey_EmptyAgentDefaultsGeneral.
func TestProjectStableKey_EmptyAgentDefaultsGeneral(t *testing.T) {
	t.Parallel()
	withEmpty := ProjectStableKey("/a/b", "")
	withGeneral := ProjectStableKey("/a/b", "general")
	if withEmpty != withGeneral {
		t.Errorf("empty agent %q != general %q", withEmpty, withGeneral)
	}
	if !strings.HasSuffix(withEmpty, ":general") {
		t.Errorf("expected :general suffix, got %q", withEmpty)
	}
}

// TestProjectStableKey_EmptyPathReturnsEmpty.
func TestProjectStableKey_EmptyPathReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := ProjectStableKey("", "general"); got != "" {
		t.Errorf("ProjectStableKey(\"\") = %q, want empty", got)
	}
}

// TestProjectStableKey_4SegmentShape: the result is a valid 4-segment key,
// platform=dashboard, chatType=pj, agent in parts[3], and passes the key
// validator.
func TestProjectStableKey_4SegmentShape(t *testing.T) {
	t.Parallel()
	key := ProjectStableKey("/home/ec2-user/workspace/naozhi", "sonnet")
	parts := strings.Split(key, ":")
	if len(parts) != 4 {
		t.Fatalf("expected 4 segments, got %d: %q", len(parts), key)
	}
	if parts[0] != "dashboard" {
		t.Errorf("platform = %q, want dashboard", parts[0])
	}
	if parts[1] != "pj" {
		t.Errorf("chatType = %q, want pj", parts[1])
	}
	if parts[3] != "sonnet" {
		t.Errorf("agent = %q, want sonnet", parts[3])
	}
	if !sessionkey.IsDashboardProjectKey(key) {
		t.Errorf("IsDashboardProjectKey(%q) = false, want true", key)
	}
	if err := ValidateSessionKey(key); err != nil {
		t.Errorf("ValidateSessionKey(%q) = %v, want nil", key, err)
	}
}

// TestProjectStableKey_AgentSanitized: a colon in the agent (key separator)
// is stripped so it cannot inject extra segments.
func TestProjectStableKey_AgentSanitized(t *testing.T) {
	t.Parallel()
	key := ProjectStableKey("/a/b", "ev:il")
	if n := strings.Count(key, ":"); n != 3 {
		t.Errorf("expected exactly 3 colons (4 segments), got %d in %q", n, key)
	}
}

// TestProjectStableKey_OverrideChangesKeyConsistently pins the workspace-
// override interaction (RFC §4.1 v2.1): changing the workspace path changes
// the key (new project = new continuation line), but the same path always
// maps back to the same key (idempotent, no split).
func TestProjectStableKey_OverrideChangesKeyConsistently(t *testing.T) {
	t.Parallel()
	orig := ProjectStableKey("/proj/a", "general")
	overridden := ProjectStableKey("/proj/b", "general")
	if orig == overridden {
		t.Fatalf("override to a different path should change the key")
	}
	// Switching back to the original path reproduces the original key.
	if back := ProjectStableKey("/proj/a", "general"); back != orig {
		t.Errorf("same path did not reproduce key: %q != %q", back, orig)
	}
}
