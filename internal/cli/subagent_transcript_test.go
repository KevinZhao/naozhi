package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTranscriptReader_MapsAllShapes pins RFC v4 §3.4.1 on-disk→EventEntry
// mapping table. Each entry below is one row in that table. Adding a row
// here without extending TranscriptReader.Read means the test fails fast.
func TestTranscriptReader_MapsAllShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		line     string
		wantType string
		wantSkip bool
	}{
		{
			name:     "user_teammate_control_skipped",
			line:     `{"type":"user","message":{"role":"user","content":"<teammate-message teammate_id=\"lister-1\" summary=\"List /tmp\">prompt body</teammate-message>"},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`,
			wantSkip: true,
		},
		{
			name:     "user_text_string",
			line:     `{"type":"user","message":{"role":"user","content":"plain text prompt"},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`,
			wantType: "text",
		},
		{
			name:     "assistant_thinking",
			line:     `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","text":"pondering"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`,
			wantType: "thinking",
		},
		{
			name:     "assistant_text",
			line:     `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"a reply"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`,
			wantType: "text",
		},
		{
			name:     "assistant_tool_use_bash",
			line:     `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls /tmp"}}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`,
			wantType: "tool_use",
		},
		{
			name:     "assistant_tool_use_Agent_downgrades_to_tool_use",
			line:     `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Agent","input":{"name":"nested","team_name":"x"}}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`,
			wantType: "tool_use",
		},
		{
			name:     "user_tool_result_string",
			line:     `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok output"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`,
			wantType: "tool_result",
		},
		{
			name:     "user_tool_result_text_array",
			line:     `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"inner"}]}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`,
			wantType: "tool_result",
		},
		{
			name:     "user_tool_result_tool_reference_only_skipped",
			line:     `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":[{"type":"tool_reference","tool_name":"SendMessage"}]}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`,
			wantSkip: true,
		},
		{
			name:     "system_api_error",
			line:     `{"type":"system","subtype":"api_error","message":{"role":"system","content":"rate limited"},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`,
			wantType: "system",
		},
		{
			name:     "system_other_skipped",
			line:     `{"type":"system","subtype":"stop_hook_summary","sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`,
			wantSkip: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tmpFile(t, tc.line+"\n")
			r := NewTranscriptReader(path)
			ents, err := r.Read(0, 100)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if tc.wantSkip {
				if len(ents) != 0 {
					t.Errorf("want 0 entries, got %+v", ents)
				}
				return
			}
			if len(ents) != 1 {
				t.Fatalf("want 1 entry, got %d (%+v)", len(ents), ents)
			}
			if ents[0].Type != tc.wantType {
				t.Errorf("Type=%q, want %q", ents[0].Type, tc.wantType)
			}
		})
	}
}

func TestTranscriptReader_PersistedPathExtracted(t *testing.T) {
	t.Parallel()
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"<persisted-output>\nOutput too large (86.9KB). Full output saved at: /home/ec2-user/.claude/projects/-foo/sessionUUID/tool-results/abc12.txt\n"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	path := tmpFile(t, line+"\n")
	r := NewTranscriptReader(path)
	ents, err := r.Read(0, 10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("len=%d", len(ents))
	}
	// PersistedPath rides in Detail prefix for now — the RFC §3.4.2 design
	// uses a dedicated field but we keep the schema change out of EventEntry
	// for this phase. Reader exposes it via extractPersistedPath helper
	// directly.
	got := extractPersistedPath("<persisted-output>\nOutput too large saved at: /home/ec2-user/.claude/projects/-foo/sessionUUID/tool-results/abc12.txt\n")
	if got != "tool-results/abc12.txt" {
		t.Errorf("extractPersistedPath=%q", got)
	}
}

func TestTranscriptReader_IncrementalRead_TrackOffset(t *testing.T) {
	t.Parallel()
	line1 := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"one"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	line2 := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"two"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:01Z"}`

	path := tmpFile(t, line1+"\n")
	r := NewTranscriptReader(path)

	ents1, err := r.Read(0, 100)
	if err != nil {
		t.Fatalf("Read1: %v", err)
	}
	if len(ents1) != 1 || ents1[0].Summary != "one" {
		t.Fatalf("first read: %+v", ents1)
	}

	// Append a second line and Tail — should only surface the new entry.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(line2 + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	ents2, err := r.Tail()
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(ents2) != 1 || ents2[0].Summary != "two" {
		t.Fatalf("tail: %+v", ents2)
	}
}

func TestTranscriptReader_PartialLine_ReassemblesOnNextRead(t *testing.T) {
	t.Parallel()
	line1 := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"aa"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	// Write first line + partial of a second line (no trailing newline yet).
	line2FirstHalf := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text"`
	line2Rest := `:"bb"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:01Z"}`

	path := tmpFile(t, line1+"\n"+line2FirstHalf)
	r := NewTranscriptReader(path)

	ents, err := r.Read(0, 100)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(ents) != 1 || ents[0].Summary != "aa" {
		t.Fatalf("partial read: %+v", ents)
	}

	// Complete the partial line and add terminator.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	f.WriteString(line2Rest + "\n")
	f.Close()

	ents2, err := r.Tail()
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(ents2) != 1 || ents2[0].Summary != "bb" {
		t.Fatalf("second read: %+v", ents2)
	}
}

func TestTranscriptReader_MalformedLineSkipped(t *testing.T) {
	t.Parallel()
	good := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	bad := `{not valid json`
	path := tmpFile(t, bad+"\n"+good+"\n")
	r := NewTranscriptReader(path)
	ents, err := r.Read(0, 100)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(ents) != 1 || ents[0].Summary != "hi" {
		t.Fatalf("malformed should be skipped, got %+v", ents)
	}
}

func tmpFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "agent-test.jsonl")
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// Guard that the teammate-message packet detector does not false-positive on
// strings that merely contain "<teammate" substring (e.g. inside thinking).
func TestTranscriptReader_TeammateDetection_ContentString(t *testing.T) {
	t.Parallel()
	// This is a thinking block that happens to mention the word "<teammate".
	// thinking content lives under assistant.content[].type=thinking, not
	// user.content, so it should NOT be skipped.
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","text":"we mention <teammate-message marker> here"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	path := tmpFile(t, line+"\n")
	r := NewTranscriptReader(path)
	ents, err := r.Read(0, 100)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("thinking should not be skipped: %+v", ents)
	}
	if !strings.Contains(ents[0].Summary, "teammate") {
		t.Errorf("Summary=%q", ents[0].Summary)
	}
}
