package persist

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestKeyHash_Deterministic ensures KeyHash(k) never drifts across
// invocations. The router relies on this at startup to locate a
// session's existing log file from its in-memory key.
func TestKeyHash_Deterministic(t *testing.T) {
	cases := []string{
		"dashboard:direct:alice:general",
		"cron:job-abc-123",
		"project:planner-xxx:planner",
		"",                        // empty
		"🔥:emoji:key:with:colons", // Unicode
		strings.Repeat("a", 4096), // large key
	}
	for _, k := range cases {
		h1 := KeyHash(k)
		h2 := KeyHash(k)
		if h1 != h2 {
			t.Errorf("non-deterministic for %q: %q vs %q", k, h1, h2)
		}
		if len(h1) != keyHashBytes*2 {
			t.Errorf("%q → %d hex chars, want %d", k, len(h1), keyHashBytes*2)
		}
		for i, c := range h1 {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("%q → non-hex char %q at %d", k, c, i)
			}
		}
	}
}

// TestKeyHash_DifferentiatesKeys confirms that similar keys yield
// different stems. Truncation to 16 bytes means 2^-64 collision
// probability, but equal prefixes would be a bug in the hash choice.
func TestKeyHash_DifferentiatesKeys(t *testing.T) {
	h1 := KeyHash("dashboard:direct:alice:general")
	h2 := KeyHash("dashboard:direct:alice:planner")
	if h1 == h2 {
		t.Errorf("near-identical keys collided: %q", h1)
	}
}

// TestLogPath_IdxPath confirms the path composition obeys
// filepath.Join semantics — no double-slashes, no missing separators.
func TestLogPath_IdxPath(t *testing.T) {
	dir := "/var/naozhi/events"
	key := "dashboard:direct:alice:general"

	log := LogPath(dir, key)
	idx := filepath.Join(dir, KeyHash(key)+idxExt)

	if filepath.Dir(log) != dir {
		t.Errorf("LogPath parent = %q, want %q", filepath.Dir(log), dir)
	}
	if filepath.Dir(idx) != dir {
		t.Errorf("IdxPath parent = %q, want %q", filepath.Dir(idx), dir)
	}
	if !strings.HasSuffix(log, ".log") {
		t.Errorf("LogPath does not end in .log: %q", log)
	}
	if !strings.HasSuffix(idx, ".idx") {
		t.Errorf("IdxPath does not end in .idx: %q", idx)
	}
	// Log and idx must share a stem so one DropKey finds both.
	logStem := strings.TrimSuffix(filepath.Base(log), ".log")
	idxStem := strings.TrimSuffix(filepath.Base(idx), ".idx")
	if logStem != idxStem {
		t.Errorf("stem mismatch: log=%q idx=%q", logStem, idxStem)
	}
}

// TestIsLogFileName covers the orphan-sweep classifier. Each row
// documents a real scenario the startup path must handle:
func TestIsLogFileName(t *testing.T) {
	stem := KeyHash("k")
	tests := []struct {
		in   string
		want bool
	}{
		{stem + ".log", true},
		{stem + ".idx", false},
		{stem + ".tmp.1700000000.log", false}, // rotate staging — not a committed file
		{stem + ".tmp.1700000000.idx", false},
		{"README.md", false},            // operator dropped a text file
		{"f" + stem[1:] + ".log", true}, // different key, still a valid hex stem
		{"not-hex.log", false},          // malformed stem
		{"", false},
		{".log", false},
	}
	for _, tc := range tests {
		if got := IsLogFileName(tc.in); got != tc.want {
			t.Errorf("IsLogFileName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestIsIdxFileName is the symmetric test for idx classification.
func TestIsIdxFileName(t *testing.T) {
	stem := KeyHash("k")
	tests := []struct {
		in   string
		want bool
	}{
		{stem + ".idx", true},
		{stem + ".log", false},
		{stem + ".tmp.1700000000.idx", false},
		{"not-hex.idx", false},
	}
	for _, tc := range tests {
		if got := IsIdxFileName(tc.in); got != tc.want {
			t.Errorf("IsIdxFileName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestIsTmpFileName ensures the startup cleanup path recognizes
// rotate-staging files so it can delete them — only a committed
// rename produces a final .log / .idx file, so any tmp is orphan.
func TestIsTmpFileName(t *testing.T) {
	stem := KeyHash("k")
	tests := []struct {
		in   string
		want bool
	}{
		{stem + ".tmp.1700000000.log", true},
		{stem + ".tmp.1700000000.idx", true},
		{stem + ".log", false},
		{stem + ".idx", false},
		{"README.md", false},
	}
	for _, tc := range tests {
		if got := IsTmpFileName(tc.in); got != tc.want {
			t.Errorf("IsTmpFileName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
