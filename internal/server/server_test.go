package server

import "testing"

func TestSplitText(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		maxLen int
		want   int // expected number of chunks
	}{
		{"short text", "hello", 100, 1},
		{"exact limit", "abcde", 5, 1},
		{"needs split", "abcdefgh", 4, 2},
		{"empty text", "", 100, 1},
		{"split at newline", "abc\ndef\nghi", 6, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := splitText(tt.text, tt.maxLen)
			if len(chunks) != tt.want {
				t.Errorf("splitText(%q, %d) = %d chunks, want %d: %v",
					tt.text, tt.maxLen, len(chunks), tt.want, chunks)
			}
			// Verify all content is preserved
			joined := ""
			for _, c := range chunks {
				joined += c
			}
			if joined != tt.text {
				t.Errorf("joined chunks = %q, want %q", joined, tt.text)
			}
		})
	}
}

func TestSplitTextLong(t *testing.T) {
	// 10 chars, maxLen=3 -> 4 chunks
	chunks := splitText("0123456789", 3)
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4: %v", len(chunks), chunks)
	}
	for i, c := range chunks {
		if len(c) > 3 {
			t.Errorf("chunk[%d] len=%d, max 3: %q", i, len(c), c)
		}
	}
}

func TestSplitTextPreferNewline(t *testing.T) {
	// "aaa\nbbb" with maxLen=5 should split at the newline
	text := "aaa\nbbb"
	chunks := splitText(text, 5)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2: %v", len(chunks), chunks)
	}
	if chunks[0] != "aaa\n" {
		t.Errorf("chunk[0] = %q, want %q", chunks[0], "aaa\n")
	}
	if chunks[1] != "bbb" {
		t.Errorf("chunk[1] = %q, want %q", chunks[1], "bbb")
	}
}

func TestParseCronAdd(t *testing.T) {
	tests := []struct {
		args         string
		wantSchedule string
		wantPrompt   string
		wantErr      bool
	}{
		{`"@every 30m" check status`, "@every 30m", "check status", false},
		{`"0 9 * * 1-5" /review scan PRs`, "0 9 * * 1-5", "/review scan PRs", false},
		{`"@daily" summarize`, "@daily", "summarize", false},
		{`"@every 1h"`, "", "", true},    // missing prompt
		{`no quotes here`, "", "", true}, // no opening quote
		{`"unclosed`, "", "", true},      // no closing quote
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			schedule, prompt, err := parseCronAdd(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseCronAdd(%q): err=%v, wantErr=%v", tt.args, err, tt.wantErr)
				return
			}
			if schedule != tt.wantSchedule {
				t.Errorf("schedule = %q, want %q", schedule, tt.wantSchedule)
			}
			if prompt != tt.wantPrompt {
				t.Errorf("prompt = %q, want %q", prompt, tt.wantPrompt)
			}
		})
	}
}

func TestResolveAgent(t *testing.T) {
	cmds := map[string]string{
		"review":   "code-reviewer",
		"research": "researcher",
	}

	tests := []struct {
		text      string
		wantAgent string
		wantText  string
	}{
		{"hello world", "general", "hello world"},
		{"/review PR#123", "code-reviewer", "PR#123"},
		{"/research quantum computing", "researcher", "quantum computing"},
		{"/review", "code-reviewer", ""},
		{"/unknown cmd", "general", "/unknown cmd"},
		{"/new", "general", "/new"},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			agent, text := resolveAgent(tt.text, cmds)
			if agent != tt.wantAgent {
				t.Errorf("resolveAgent(%q).agent = %q, want %q", tt.text, agent, tt.wantAgent)
			}
			if text != tt.wantText {
				t.Errorf("resolveAgent(%q).text = %q, want %q", tt.text, text, tt.wantText)
			}
		})
	}
}
