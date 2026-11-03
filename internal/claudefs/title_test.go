package claudefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTranscriptTitles(t *testing.T) {
	t.Parallel()
	const (
		custom1 = `{"type":"custom-title","customTitle":"first","sessionId":"s"}`
		custom2 = `{"type":"custom-title","customTitle":"renamed","sessionId":"s"}`
		aiTitle = `{"type":"ai-title","aiTitle":"Fix the build","sessionId":"s"}`
		mention = `{"type":"user","message":{"role":"user","content":"a \"custom-title\" here"},"sessionId":"s"}`
	)
	cases := []struct {
		name, body         string
		wantCustom, wantAI string
	}{
		{"none", ttLines(ttUserPrompt, ttEndTurn), "", ""},
		{"last custom wins", ttLines(custom1, ttUserPrompt, custom2, ttEndTurn, ttLastPrompt), "renamed", ""},
		{"ai only", ttLines(aiTitle, ttUserPrompt, ttEndTurn), "", "Fix the build"},
		{"both", ttLines(custom1, aiTitle, ttEndTurn), "first", "Fix the build"},
		{"mention is no title", ttLines(mention), "", ""},
		{"past the first window", ttLines(custom1) + bulk(2*turnTailWindow) + ttLines(ttEndTurn), "first", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "s.jsonl")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			custom, ai, err := TranscriptTitles(path)
			if err != nil || custom != tc.wantCustom || ai != tc.wantAI {
				t.Errorf("TranscriptTitles = %q, %q, %v; want %q, %q", custom, ai, err, tc.wantCustom, tc.wantAI)
			}
		})
	}
}
