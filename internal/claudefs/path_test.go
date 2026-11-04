package claudefs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLocateSessionJSONL: the CLI writes under the slug of its cwd's
// realpath; a transcript under the slug as spelled (older data) is still
// found.
func TestLocateSessionJSONL(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(base, "claude")
	realWS := filepath.Join(base, "real", "ws")
	if err := os.MkdirAll(realWS, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(base, "real"), link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	linkWS := filepath.Join(link, "ws")
	const sid = "0b9c2f4e-1d2a-4c3b-8e5f-6a7b8c9d0e1f"
	at := func(ws string) string { return SessionJSONL(claudeDir, ws, sid) }
	tests := []struct {
		name    string
		cwd     string
		written []string // transcripts on disk
		want    string
	}{
		{"symlinked cwd, written under the realpath", linkWS, []string{at(realWS)}, at(realWS)},
		{"symlinked cwd, written as spelled", linkWS, []string{at(linkWS)}, at(linkWS)},
		{"symlinked cwd, both", linkWS, []string{at(realWS), at(linkWS)}, at(realWS)},
		{"symlinked cwd, none yet", linkWS, nil, at(realWS)},
		{"plain cwd", realWS, []string{at(realWS)}, at(realWS)},
		{"cwd gone", filepath.Join(base, "gone"), nil, at(filepath.Join(base, "gone"))},
		{"no claude dir", realWS, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.RemoveAll(claudeDir); err != nil {
				t.Fatal(err)
			}
			for _, p := range tt.written {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dir := claudeDir
			if tt.want == "" {
				dir = ""
			}
			if got := LocateSessionJSONL(dir, tt.cwd, sid); got != tt.want {
				t.Errorf("LocateSessionJSONL = %s, want %s", got, tt.want)
			}
		})
	}
}
