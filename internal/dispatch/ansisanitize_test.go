package dispatch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestStripANSI_AllClasses exercises every escape class the regex is
// supposed to cover. Each row pins the contract that the sanitizer leaves
// only the visible bytes; if a future refactor regresses one of the
// alternation arms (DCS / SOS / PM / APC are easy to drop accidentally
// because tool output rarely contains them), the matching row will fail
// in isolation so the operator can see exactly which arm broke.
func TestStripANSI_AllClasses(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "hello world", "hello world"},
		{"empty string", "", ""},
		{"CSI color", "\x1b[31mred\x1b[0m", "red"},
		{"CSI clear screen", "before\x1b[2Jafter", "beforeafter"},
		{"CSI with intermediate", "\x1b[?25hcursor", "cursor"},
		{"OSC hyperlink BEL terminated", "go \x1b]8;;https://example.com\x07link\x1b]8;;\x07", "go link"},
		{"OSC hyperlink ST terminated", "go \x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\", "go link"},
		{"OSC title", "\x1b]0;window title\x07body", "body"},
		{"DCS sixel ST terminated", "pre\x1bPq#0;0;0;0;0;0\x1b\\post", "prepost"},
		{"DCS Kitty terminfo", "\x1bP+q544e\x1b\\rest", "rest"},
		{"SOS payload", "\x1bXarbitrary\x1b\\tail", "tail"},
		{"PM privacy message", "\x1b^private\x1b\\visible", "visible"},
		{"APC Kitty image", "before\x1b_Gpayload=base64\x1b\\after", "beforeafter"},
		{"ESC single final", "\x1b=keypad", "keypad"},
		{"ESC charset designator", "\x1b(Btext", "text"},
		{"mixed CSI and OSC", "\x1b[1mbold\x1b[0m \x1b]8;;u\x07link\x1b]8;;\x07", "bold link"},
		{"mixed DCS and CSI", "\x1bPq...\x1b\\then \x1b[31mred\x1b[0m", "then red"},
		{"plain text with high bytes preserved", "中文内容", "中文内容"},
		{"plain text with newline preserved", "line1\nline2", "line1\nline2"},
		{"plain text with tab preserved", "col1\tcol2", "col1\tcol2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripANSI(tc.in)
			if got != tc.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestStripANSI_NoEscFastPath asserts that strings without any ESC byte
// take the fast-path return (no regex engine work). Verified by equality:
// Go strings are immutable, so a same-content return guarantees the
// caller sees the same backing bytes when the fast-path fires.
func TestStripANSI_NoEscFastPath(t *testing.T) {
	in := "no escapes here, even with 中文 and tabs\t"
	got := stripANSI(in)
	if got != in {
		t.Fatalf("stripANSI returned different string for ANSI-free input: got %q want %q", got, in)
	}
}

// TestFormatEventLine_StripsANSIInThinking pins #836: when a thinking event
// echoes terminal output (e.g. the model relays Bash stdout containing
// hyperlink escapes), the IM banner gets only the visible characters.
func TestFormatEventLine_StripsANSIInThinking(t *testing.T) {
	ev := cli.Event{
		Message: &cli.AssistantMessage{
			Content: []cli.ContentBlock{
				{Type: "thinking", Text: "checking \x1b]8;;https://x\x07docs\x1b]8;;\x07 now"},
			},
		},
	}
	got := formatEventLine(ev)
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("formatEventLine leaked ESC byte: %q", got)
	}
	if !strings.Contains(got, "docs") {
		t.Fatalf("formatEventLine dropped visible content: %q", got)
	}
}

// TestFormatEventLine_StripsANSIInBashCommand pins #836 for the tool_use
// arm: a Bash command containing an OSC hyperlink (which can happen when
// the model copy-pastes an example from a transcript that contained
// terminal escapes) reaches IM clean. The JSON wire shape is built via
// json.Marshal so the embedded ESC / BEL bytes are properly escaped on
// the wire and decoded back to literal control bytes by the dispatch
// json.Unmarshal — raw 0x1B inside a JSON string literal is invalid JSON.
func TestFormatEventLine_StripsANSIInBashCommand(t *testing.T) {
	cmd := "echo \x1b]8;;u\x07hi\x1b]8;;\x07 && ls"
	input, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: cmd})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	ev := cli.Event{
		Message: &cli.AssistantMessage{
			Content: []cli.ContentBlock{
				{Type: "tool_use", Name: "Bash", Input: input},
			},
		},
	}
	got := formatEventLine(ev)
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("formatEventLine leaked ESC byte: %q", got)
	}
	if !strings.Contains(got, "hi") || !strings.Contains(got, "ls") {
		t.Fatalf("formatEventLine dropped visible Bash command content: %q", got)
	}
}
