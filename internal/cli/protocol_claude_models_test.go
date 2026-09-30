package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestUsableSettingsFile(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/abs/settings.json", "/abs/settings.json"},
		{"", ""},
		{"relative/settings.json", ""},
		{"-injected", ""},
	}
	for _, tc := range cases {
		if got := usableSettingsFile(tc.in); got != tc.want {
			t.Errorf("usableSettingsFile(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestClaudeAvailableModels_ReadsTheFileBuildArgsChose pins the facet the
// dashboard model popover reads: the list must come from the settings file this
// process actually spawned with, not from any global default.
func TestClaudeAvailableModels_ReadsTheFileBuildArgsChose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"availableModels":["claude-opus-5[1m]","claude-opus-5"]}`)

	p := &ClaudeProtocol{}
	if got := p.BuildArgs(SpawnOptions{SettingsFile: path}); !containsPair(got, "--settings", path) {
		t.Fatalf("BuildArgs did not point cc at the file: %v", got)
	}
	want := []ModelInfo{{ID: "claude-opus-5[1m]"}, {ID: "claude-opus-5"}}
	if got := p.AvailableModels(); !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableModels() = %+v, want %+v", got, want)
	}
}

// TestClaudeAvailableModels_RereadsAfterTheFileChanges is why `naozhi models
// sync -write` does not need a restart to reach a live process.
func TestClaudeAvailableModels_RereadsAfterTheFileChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"availableModels":["claude-opus-5"]}`)
	p := &ClaudeProtocol{}
	p.BuildArgs(SpawnOptions{SettingsFile: path})
	if got := p.AvailableModels(); len(got) != 1 {
		t.Fatalf("AvailableModels() = %+v", got)
	}
	writeSettings(t, path, `{"availableModels":["claude-opus-5","claude-sonnet-5"]}`)
	if got := p.AvailableModels(); len(got) != 2 {
		t.Errorf("AvailableModels() = %+v, want the updated list", got)
	}
}

// TestClaudeAvailableModels_NilLetsLowerTiersWin: nil, not an empty slice, is
// what makes BackendModelManifest fall through to the configured list.
func TestClaudeAvailableModels_NilLetsLowerTiersWin(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"no key":        `{"outputStyle":"Concise"}`,
		"empty list":    `{"availableModels":[]}`,
		"blank entries": `{"availableModels":["","  "]}`,
		"malformed":     `{"availableModels":`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			writeSettings(t, path, body)
			p := &ClaudeProtocol{}
			p.BuildArgs(SpawnOptions{SettingsFile: path})
			if got := p.AvailableModels(); got != nil {
				t.Errorf("AvailableModels() = %+v, want nil", got)
			}
		})
	}
}

func TestClaudeAvailableModels_MissingFileIsNotAnError(t *testing.T) {
	p := &ClaudeProtocol{}
	p.BuildArgs(SpawnOptions{SettingsFile: filepath.Join(t.TempDir(), "absent.json")})
	if got := p.AvailableModels(); got != nil {
		t.Errorf("AvailableModels() = %+v, want nil", got)
	}
}

// TestClaudeClone_DoesNotInheritTheCache: a resumed process may spawn against a
// different settings file, so the clone must resolve its own.
func TestClaudeClone_DoesNotInheritTheCache(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.json")
	second := filepath.Join(dir, "b.json")
	writeSettings(t, first, `{"availableModels":["claude-opus-5"]}`)
	writeSettings(t, second, `{"availableModels":["claude-sonnet-5"]}`)

	p := &ClaudeProtocol{}
	p.BuildArgs(SpawnOptions{SettingsFile: first})
	_ = p.AvailableModels()

	clone, ok := p.Clone().(*ClaudeProtocol)
	if !ok {
		t.Fatal("Clone did not return a *ClaudeProtocol")
	}
	clone.BuildArgs(SpawnOptions{SettingsFile: second})
	if got := clone.AvailableModels(); len(got) != 1 || got[0].ID != "claude-sonnet-5" {
		t.Errorf("clone AvailableModels() = %+v, want the second file's list", got)
	}
}

// writeSettings writes body and nudges mtime forward, because the cache's change
// token is mtime+size and a test can rewrite a file within one timestamp tick.
func writeSettings(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	now := time.Now().Add(time.Duration(len(body)) * time.Millisecond)
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
