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

// TestClaudeAvailableModels_UnknownSourceIsNil: a reattached process never ran
// BuildArgs, so guessing ~/.claude/settings.json would hand the dashboard the
// interactive cc's list instead of the one this process enforces.
func TestClaudeAvailableModels_UnknownSourceIsNil(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"availableModels":["claude-opus-5"]}`)

	p, ok := (&ClaudeProtocol{}).Clone().(*ClaudeProtocol)
	if !ok {
		t.Fatal("Clone did not return a *ClaudeProtocol")
	}
	if got := p.AvailableModels(); got != nil {
		t.Errorf("AvailableModels() = %+v, want nil for an unknown settings source", got)
	}
}

// TestClaudeAvailableModels_ReattachSeedsFromArgv: the shim-recorded argv is
// enough to recover either settings source BuildArgs can choose.
func TestClaudeAvailableModels_ReattachSeedsFromArgv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"), `{"availableModels":["claude-opus-5"]}`)
	owned := filepath.Join(t.TempDir(), "naozhi-settings.json")
	writeSettings(t, owned, `{"availableModels":["claude-opus-5-5[1m]","claude-sonnet-5-5[1m]"]}`)

	cases := map[string]struct {
		opts SpawnOptions
		want []ModelInfo
	}{
		"naozhi-owned file": {SpawnOptions{SettingsFile: owned}, []ModelInfo{{ID: "claude-opus-5-5[1m]"}, {ID: "claude-sonnet-5-5[1m]"}}},
		"user settings":     {SpawnOptions{}, []ModelInfo{{ID: "claude-opus-5"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			argv := (&ClaudeProtocol{}).BuildArgs(tc.opts)
			p := &ClaudeProtocol{}
			p.seedSettingsFromArgs(argv)
			if got := p.AvailableModels(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("AvailableModels() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestClaudeSeedSettings_FillIfUnset: a source BuildArgs already recorded wins.
func TestClaudeSeedSettings_FillIfUnset(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.json")
	writeSettings(t, first, `{"availableModels":["claude-opus-5"]}`)
	p := &ClaudeProtocol{}
	p.BuildArgs(SpawnOptions{SettingsFile: first})
	p.seedSettingsFromArgs([]string{"--settings", filepath.Join(dir, "b.json")})
	if got := p.AvailableModels(); len(got) != 1 || got[0].ID != "claude-opus-5" {
		t.Errorf("AvailableModels() = %+v, want the BuildArgs file's list", got)
	}
}

func TestSettingsSourceFromArgs(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		want   string
		wantOK bool
	}{
		{"owned file", []string{"-p", "--setting-sources", "", "--settings", "/abs/s.json", "--verbose"}, "/abs/s.json", true},
		{"user", []string{"--setting-sources", "user"}, localSettingsMarker, true},
		{"absent", []string{"-p", "--verbose"}, "", false},
		{"dangling", []string{"--settings"}, "", false},
		{"relative file ignored", []string{"--settings", "rel.json"}, "", false},
		{"flag-shaped file ignored", []string{"--settings", "-x"}, "", false},
		{"last wins", []string{"--setting-sources", "user", "--settings", "/abs/s.json"}, "/abs/s.json", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := settingsSourceFromArgs(tc.args)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("settingsSourceFromArgs(%q) = (%q, %v), want (%q, %v)", tc.args, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestProcess_SeedFromSpawnArgs_Settings reaches the protocol through the
// Process method the reconnect path calls.
func TestProcess_SeedFromSpawnArgs_Settings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"availableModels":["claude-sonnet-5-5[1m]"]}`)
	p, srv := shimTestPair(&ClaudeProtocol{})
	defer srv.conn.Close()
	p.SeedFromSpawnArgs([]string{"--setting-sources", "", "--settings", path})
	if got := p.AvailableModels(); len(got) != 1 || got[0].ID != "claude-sonnet-5-5[1m]" {
		t.Errorf("AvailableModels() = %+v, want the argv file's list", got)
	}

	// ACP reports its own manifest; a --settings flag in argv must not change it.
	acp := &ACPProtocol{BackendID: "kiro"}
	q, srv2 := shimTestPair(acp)
	defer srv2.conn.Close()
	before := acp.AvailableModels()
	q.SeedFromSpawnArgs([]string{"--settings", path})
	if got := q.AvailableModels(); !reflect.DeepEqual(got, before) {
		t.Errorf("ACP AvailableModels() = %+v after seeding, want unchanged %+v", got, before)
	}
}
