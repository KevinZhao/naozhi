package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizeClaudeUUID guards the dash-stripping and length
// gate. MergedSource compares UUIDs byte-for-byte, so any drift
// between "Claude emits 3f758de9-73de-... but naozhi stores the
// dashless form" and this helper would silently break dedup.
func TestNormalizeClaudeUUID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"3f758de9-73de-48ea-8063-b345d2011601", "3f758de973de48ea8063b345d2011601"},
		{"AABBCCDDEEFFGGHH0011223344556677", ""}, // not all hex
		{"aabb", ""},                             // too short
		{"", ""},
		// Upper-case hex canonicalised to lower.
		{"3F758DE973DE48EA8063B345D2011601", "3f758de973de48ea8063b345d2011601"},
	}
	for _, tc := range tests {
		if got := normalizeClaudeUUID(tc.in); got != tc.want {
			t.Errorf("normalizeClaudeUUID(%q) = %q, want %q",
				tc.in, got, tc.want)
		}
	}
}

// TestLoadHistory_UUIDFromClaudeJSONL feeds a realistic JSONL
// fixture and confirms the resulting EventEntry.UUID mirrors the
// line's uuid field (dash-stripped). Anchors the core dedup path.
func TestLoadHistory_UUIDFromClaudeJSONL(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, "claude")
	projectsDir := filepath.Join(claudeDir, "projects", "-home-ec2-user")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	sessionID := "11111111-2222-3333-4444-555555555555"
	jsonlPath := filepath.Join(projectsDir, sessionID+".jsonl")
	content := strings.Join([]string{
		`{"type":"user","uuid":"d8c544b3-8b05-4df0-94c4-bc82ffecad1d","timestamp":"2026-04-18T13:58:18.246Z","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","uuid":"abcd1234-5678-90ab-cdef-1122334455aa","timestamp":"2026-04-18T13:58:19.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hi back"}]}}`,
	}, "\n")
	if err := os.WriteFile(jsonlPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := LoadHistory(claudeDir, sessionID, "/home/ec2-user")
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	// user row: UUID adopted verbatim (dashless).
	if entries[0].UUID != "d8c544b38b054df094c4bc82ffecad1d" {
		t.Errorf("user UUID=%q", entries[0].UUID)
	}
	// assistant row: first block inherits line UUID.
	if entries[1].UUID != "abcd1234567890abcdef1122334455aa" {
		t.Errorf("assistant UUID=%q", entries[1].UUID)
	}
}

// TestLoadHistory_UUIDFallback_NoClaudeUUID drives the fallback:
// line without a uuid field must still produce a stable (and
// deterministic) UUID via DeriveLegacyUUID so MergedSource can dedup
// against a separately-persisted naozhi-native copy.
func TestLoadHistory_UUIDFallback_NoClaudeUUID(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, "claude")
	projectsDir := filepath.Join(claudeDir, "projects", "-home-ec2-user")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	sessionID := "ffffffff-0000-1111-2222-333333333333"
	jsonlPath := filepath.Join(projectsDir, sessionID+".jsonl")
	// Deliberately omit "uuid" from the line — older Claude CLI
	// builds or hand-crafted fixtures.
	content := `{"type":"user","timestamp":"2026-04-18T13:58:18.246Z","message":{"role":"user","content":"hello"}}`
	if err := os.WriteFile(jsonlPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Two calls with the same file must produce the same UUID.
	first, err := LoadHistory(claudeDir, sessionID, "/home/ec2-user")
	if err != nil {
		t.Fatalf("LoadHistory 1: %v", err)
	}
	second, err := LoadHistory(claudeDir, sessionID, "/home/ec2-user")
	if err != nil {
		t.Fatalf("LoadHistory 2: %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("unexpected counts: %d vs %d", len(first), len(second))
	}
	if first[0].UUID == "" {
		t.Errorf("fallback UUID empty")
	}
	if first[0].UUID != second[0].UUID {
		t.Errorf("fallback UUID not stable: %q vs %q",
			first[0].UUID, second[0].UUID)
	}
	// Length matches the naozhi-native shape (32 lowercase hex).
	if len(first[0].UUID) != 32 {
		t.Errorf("UUID length=%d, want 32", len(first[0].UUID))
	}
}

// TestLoadHistory_MultiBlockAssistant_DistinctUUIDs: when one
// assistant line carries 2+ text blocks, each block gets a distinct
// UUID so the dashboard can render them as separate bubbles without
// merging via dedup.
func TestLoadHistory_MultiBlockAssistant_DistinctUUIDs(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, "claude")
	projectsDir := filepath.Join(claudeDir, "projects", "-home-ec2-user")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	jsonlPath := filepath.Join(projectsDir, sessionID+".jsonl")
	content := `{"type":"assistant","uuid":"00000000-0000-0000-0000-000000000001","timestamp":"2026-04-18T13:58:18.246Z","message":{"role":"assistant","content":[{"type":"text","text":"first block"},{"type":"text","text":"second block"}]}}`
	if err := os.WriteFile(jsonlPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := LoadHistory(claudeDir, sessionID, "/home/ec2-user")
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d, want 2 blocks", len(entries))
	}
	if entries[0].UUID == "" || entries[1].UUID == "" {
		t.Fatalf("UUID missing on multi-block: %+v", entries)
	}
	if entries[0].UUID == entries[1].UUID {
		t.Errorf("multi-block UUIDs collided: %q", entries[0].UUID)
	}
	// First block inherits the line UUID verbatim.
	if entries[0].UUID != "00000000000000000000000000000001" {
		t.Errorf("first block UUID=%q, want line-uuid", entries[0].UUID)
	}
}
