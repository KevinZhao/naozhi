//go:build unix

package project

import (
	"os"
	"path/filepath"
	"testing"
)

// gitTracksStub reads the answer from what git lists, not from its exit
// code: a git that fails the way the macOS CLT shim does (stderr, exit 1) is
// trackUnknown. GIT_* variables in naozhi's environment do not reach git.
func TestGitTracksStub_FakeGit(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       trackState
	}{
		{"listed", `printf '.naozhi/project.yaml\0'`, trackTracked},
		{"nothing listed", `exit 0`, trackUntracked},
		{"shim exit 1", `echo "xcrun: error: invalid active developer path" >&2; exit 1`, trackUnknown},
		{"not a repo", `echo "fatal: not a git repository" >&2; exit 128`, trackUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			script := "#!/bin/sh\n[ -n \"${GIT_DIR+x}\" ] && exit 0\n" + tc.body + "\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("GIT_DIR", "/elsewhere/.git")
			if got := gitTracksStub(t.TempDir()); got != tc.want {
				t.Errorf("gitTracksStub = %d; want %d", got, tc.want)
			}
		})
	}
}
