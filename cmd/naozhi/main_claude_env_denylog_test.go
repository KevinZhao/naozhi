package main

import (
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/spawndiag"
)

// collectDiags installs a diag observer for the duration of a test and returns
// the collected diags keyed by the env var they name.
func collectDiags(t *testing.T) map[string]spawndiag.Diag {
	t.Helper()
	var mu sync.Mutex
	got := map[string]spawndiag.Diag{}
	restore := spawndiag.Observe(func(_ string, d spawndiag.Diag) {
		mu.Lock()
		got[d.Key] = d
		mu.Unlock()
	})
	t.Cleanup(restore)
	return got
}

// TestFilterClaudeEnv_DenyDiagPerKey pins the report per denied key. The reason
// is derived from the key's namespace; if a future envpolicy Table edit denies a
// settings key outside AWS_/CLAUDE_, this forces the wording (and the heuristic)
// to be revisited rather than silently mislabelling the rejection. It also pins
// the silent case: a key outside the allowed namespaces is everyday noise, not a
// gate rejection, and must produce no diag at all.
//
// Not parallel: the diag observer is process-global.
func TestFilterClaudeEnv_DenyDiagPerKey(t *testing.T) {
	got := collectDiags(t)

	kept := filterClaudeEnv(map[string]string{
		"AWS_PROFILE":                              "admin",
		"AWS_SHARED_CREDENTIALS_FILE":              "/x",
		"CLAUDE_CODE_USE_MOCK_RESPONSES":           "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"UNRELATED_VAR":                            "silent-skip", // outside namespaces: no diag at all
	})
	if len(kept) != 0 {
		t.Fatalf("all inputs must be dropped, got %v", kept)
	}

	want := map[string]string{
		"AWS_PROFILE":                              "auth-source AWS var",
		"AWS_SHARED_CREDENTIALS_FILE":              "auth-source AWS var",
		"CLAUDE_CODE_USE_MOCK_RESPONSES":           "CLAUDE_ kill-switch var",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "CLAUDE_ kill-switch var",
	}
	for key, reasonPart := range want {
		d, ok := got[key]
		if !ok {
			t.Errorf("no diag reported for denied key %q", key)
			continue
		}
		if d.Layer != "env-filter" || d.Action != "dropped" {
			t.Errorf("diag for %q = layer %q action %q, want env-filter/dropped", key, d.Layer, d.Action)
		}
		if !strings.Contains(d.Reason, reasonPart) {
			t.Errorf("reason for %q = %q, want it to say %q", key, d.Reason, reasonPart)
		}
	}
	if d, ok := got["UNRELATED_VAR"]; ok {
		t.Errorf("key outside allowed namespaces must be skipped silently, reported %+v", d)
	}
}

// TestFilterClaudeEnv_UnsafeValueDiags covers the two value-level gates: an
// execve-hostile value and a base URL that fails its guard. Both are drops the
// operator has to be able to see.
func TestFilterClaudeEnv_UnsafeValueDiags(t *testing.T) {
	got := collectDiags(t)

	kept := filterClaudeEnv(map[string]string{
		"ANTHROPIC_MODEL":    "claude\nsonnet", // newline: must never reach execve
		"ANTHROPIC_BASE_URL": "http://evil.test",
	})
	if len(kept) != 0 {
		t.Fatalf("both inputs must be dropped, got %v", kept)
	}
	if d, ok := got["ANTHROPIC_MODEL"]; !ok {
		t.Error("no diag for a value carrying a newline")
	} else if !strings.Contains(d.Reason, "unsafe for execve") {
		t.Errorf("reason = %q, want it to name the execve hazard", d.Reason)
	}
	if d, ok := got["ANTHROPIC_BASE_URL"]; !ok {
		t.Error("no diag for a base_url that fails its guard")
	} else if !strings.Contains(d.Reason, "guard") {
		t.Errorf("reason = %q, want it to name the failed guard", d.Reason)
	}
}
