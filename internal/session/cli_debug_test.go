package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// stubGetenv builds a getenv func returning v for cliDebugEnvVar only.
func stubGetenv(v string) func(string) string {
	return func(k string) string {
		if k == cliDebugEnvVar {
			return v
		}
		return ""
	}
}

func TestResolveCLIDebugDir_OffWhenEnvUnset(t *testing.T) {
	eventDir := filepath.Join(t.TempDir(), "events")
	for _, v := range []string{"", "0", "false", "off", "no"} {
		if got := resolveCLIDebugDirWith(eventDir, stubGetenv(v)); got != "" {
			t.Errorf("env=%q: want disabled (\"\"), got %q", v, got)
		}
	}
}

func TestResolveCLIDebugDir_OnCreatesSiblingDir(t *testing.T) {
	dataDir := t.TempDir()
	eventDir := filepath.Join(dataDir, "events")

	got := resolveCLIDebugDirWith(eventDir, stubGetenv("1"))

	want := filepath.Join(dataDir, "cli-debug")
	if got != want {
		t.Fatalf("debug dir = %q, want %q (sibling of events under same data root)", got, want)
	}
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatalf("debug dir not created: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("debug path is not a directory")
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("debug dir mode = %o, want 0700", perm)
	}
}

func TestResolveCLIDebugDir_OffWhenEventLogDisabled(t *testing.T) {
	// Opt-in present but no event log dir → no data root to anchor under.
	if got := resolveCLIDebugDirWith("", stubGetenv("1")); got != "" {
		t.Errorf("event log disabled: want \"\", got %q", got)
	}
}

func TestResolveCLIDebugDir_TruthyValues(t *testing.T) {
	dataDir := t.TempDir()
	eventDir := filepath.Join(dataDir, "events")
	for _, v := range []string{"1", "true", "yes", "on", "debug"} {
		got := resolveCLIDebugDirWith(eventDir, stubGetenv(v))
		if got == "" {
			t.Errorf("env=%q: want enabled, got disabled", v)
		}
	}
}

func TestCLIDebugFileFor(t *testing.T) {
	key := "dashboard:direct:2026-06-09-naozhi:general"

	// Disabled router → empty path regardless of key.
	rOff := &Router{cliDebugDir: ""}
	if p := rOff.cliDebugFileFor(key); p != "" {
		t.Errorf("disabled: want \"\", got %q", p)
	}

	// Enabled router → <dir>/<keyhash>.log, stem identical to the event log.
	dir := t.TempDir()
	rOn := &Router{cliDebugDir: dir}
	got := rOn.cliDebugFileFor(key)
	want := filepath.Join(dir, persist.KeyHash(key)+".log")
	if got != want {
		t.Errorf("debug file = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, dir+string(os.PathSeparator)) {
		t.Errorf("debug file %q escapes debug dir %q", got, dir)
	}
}
