package discovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// writeJSONLFile writes JSONL lines to path.
func writeJSONLFile(t *testing.T, path string, lines []string) {
	t.Helper()
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write jsonl %s: %v", path, err)
	}
}

// makeClaudeDir creates a minimal claude dir with the project sub-tree.
// Returns the claudeDir path.
func makeClaudeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// makeSessionJSONL creates a project dir and JSONL file inside a claude dir.
// Returns projDir and the JSONL path.
func makeSessionJSONL(t *testing.T, claudeDir, dirName, sessionID string, lines []string) (string, string) {
	t.Helper()
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(projDir, sessionID+".jsonl")
	writeJSONLFile(t, jsonlPath, lines)
	return projDir, jsonlPath
}

// jsonlLine returns a single JSONL line for the given type/role/content.
func userJSONLLine(t string, content string) string {
	msg, _ := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: "user", Content: content})
	return fmt.Sprintf(`{"type":%q,"timestamp":"2026-01-01T00:00:00Z","message":%s}`,
		t, string(msg))
}

func assistantJSONLLine(text string) string {
	blocks, _ := json.Marshal([]struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{Type: "text", Text: text}})
	msg, _ := json.Marshal(struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}{Role: "assistant", Content: blocks})
	return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-01-01T00:01:00Z","message":%s}`,
		string(msg))
}

// ---------------------------------------------------------------------------
// parseTimestamp
// ---------------------------------------------------------------------------

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		input string
		wantZ bool // non-zero result expected
	}{
		{"RFC3339", "2026-01-01T00:00:00Z", true},
		{"RFC3339Nano", "2026-01-01T00:00:00.123456789Z", true},
		{"empty", "", false},
		{"garbage", "not-a-date", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTimestamp(tc.input)
			if tc.wantZ && got == 0 {
				t.Errorf("parseTimestamp(%q) = 0, want non-zero", tc.input)
			}
			if !tc.wantZ && got != 0 {
				t.Errorf("parseTimestamp(%q) = %d, want 0", tc.input, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// extractText
// ---------------------------------------------------------------------------

func TestExtractText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "plain string",
			raw:  `"hello world"`,
			want: "hello world",
		},
		{
			name: "array of blocks",
			raw:  `[{"type":"text","text":"block1"},{"type":"text","text":"block2"}]`,
			want: "block1\nblock2",
		},
		{
			name: "array with non-text blocks",
			raw:  `[{"type":"tool_use","name":"bash"},{"type":"text","text":"visible"}]`,
			want: "visible",
		},
		{
			name: "empty",
			raw:  `""`,
			want: "",
		},
		{
			name: "null",
			raw:  `null`,
			want: "",
		},
		{
			name: "empty array",
			raw:  `[]`,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractText(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("extractText(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// LoadHistory / findSessionJSONL / parseJSONL
// ---------------------------------------------------------------------------

func TestLoadHistory_EmptyDir(t *testing.T) {
	dir := makeClaudeDir(t)
	entries, err := LoadHistory(dir, "00000000-0000-0000-0000-000000000001", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty entries, got %d", len(entries))
	}
}

func TestLoadHistory_MissingClaudeDir(t *testing.T) {
	entries, err := LoadHistory("/nonexistent/path", "00000000-0000-0000-0000-000000000001", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries, got %v", entries)
	}
}

func TestLoadHistory_WithCWD(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/myproject"
	sessionID := "00000000-0000-0000-0000-000000000042"
	dirName := projDirName(cwd) // "-tmp-myproject"

	lines := []string{
		userJSONLLine("user", "hello from user"),
		assistantJSONLLine("hello from assistant"),
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	entries, err := LoadHistory(claudeDir, sessionID, cwd)
	if err != nil {
		t.Fatalf("LoadHistory error: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one entry, got 0")
	}
	// First entry must be the user message
	if entries[0].Type != "user" {
		t.Errorf("first entry type = %q, want user", entries[0].Type)
	}
	if entries[0].Summary != "hello from user" {
		t.Errorf("first entry summary = %q", entries[0].Summary)
	}
}

func TestLoadHistory_FallbackScan(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	sessionID := "00000000-0000-0000-0000-000000000043"
	// The actual project dir name is irrelevant for the scan fallback
	dirName := "-some-other-project"

	lines := []string{
		userJSONLLine("user", "scan fallback prompt"),
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	// Pass empty cwd so LoadHistory falls back to scan.
	entries, err := LoadHistory(claudeDir, sessionID, "")
	if err != nil {
		t.Fatalf("LoadHistory error: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected entries from scan fallback, got none")
	}
	if entries[0].Summary != "scan fallback prompt" {
		t.Errorf("entry summary = %q", entries[0].Summary)
	}
}

func TestLoadHistory_IgnoresMalformedLines(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/malformed"
	sessionID := "00000000-0000-0000-0000-000000000099"
	dirName := projDirName(cwd)

	lines := []string{
		"not json at all",
		`{"type":"unknown","timestamp":"2026-01-01T00:00:00Z"}`,
		userJSONLLine("user", "valid prompt"),
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	entries, err := LoadHistory(claudeDir, sessionID, cwd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Type != "user" {
		t.Errorf("entry type = %q, want user", entries[0].Type)
	}
}

func TestLoadHistory_AssistantBlocks(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/assistant"
	sessionID := "00000000-0000-0000-0000-000000000044"
	dirName := projDirName(cwd)

	lines := []string{
		assistantJSONLLine("assistant reply text"),
	}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	entries, err := LoadHistory(claudeDir, sessionID, cwd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d (lines: %v)", len(entries), lines)
	}
	if entries[0].Type != "text" {
		t.Errorf("entry type = %q, want text", entries[0].Type)
	}
}

func TestLoadHistory_Truncation(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/trunc"
	sessionID := "00000000-0000-0000-0000-000000000045"
	dirName := projDirName(cwd)

	// Build a string longer than 120 runes
	longText := ""
	for i := 0; i < 200; i++ {
		longText += "a"
	}
	lines := []string{userJSONLLine("user", longText)}
	makeSessionJSONL(t, claudeDir, dirName, sessionID, lines)

	entries, err := LoadHistory(claudeDir, sessionID, cwd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected entries")
	}
	got := entries[0].Summary
	// TruncateRunes at 120 produces "aaa...aaa" (120 rune max, appends "...")
	want := cli.TruncateRunes(longText, 120)
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestParseJSONL_EmptyLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	// file with empty lines and only whitespace
	content := "\n\n\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := parseJSONL(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestLoadHistory_UserBlockContent(t *testing.T) {
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/blockuser"
	sessionID := "00000000-0000-0000-0000-000000000046"
	dirName := projDirName(cwd)

	// User message with block content instead of plain string
	blocks, _ := json.Marshal([]struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{Type: "text", Text: "block user content"}})
	msg, _ := json.Marshal(struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}{Role: "user", Content: blocks})
	line := fmt.Sprintf(`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":%s}`, string(msg))
	makeSessionJSONL(t, claudeDir, dirName, sessionID, []string{line})

	entries, err := LoadHistory(claudeDir, sessionID, cwd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected entries")
	}
	if entries[0].Summary != "block user content" {
		t.Errorf("summary = %q", entries[0].Summary)
	}
}
