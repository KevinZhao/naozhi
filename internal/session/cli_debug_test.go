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

// TestResolveCLIDebugDir_RelativeEventLogAnchoredAbsolute guards SEC-8
// (#2133): a relatively-configured EventLogDir must not yield a relative
// debug dir, because the path is passed to the CLI subprocess as
// --debug-file and a relative value resolves against the subprocess CWD
// (the session workspace), leaking the API-key-bearing debug log there.
func TestResolveCLIDebugDir_RelativeEventLogAnchoredAbsolute(t *testing.T) {
	// Anchor the process CWD to a temp dir so EnsureDir (which resolves the
	// relative config against the CWD via filepath.Abs) creates its tree
	// there instead of polluting the source tree. t.Chdir restores the CWD
	// and removes the temp dir at test end, keeping the test hermetic.
	t.Chdir(t.TempDir())

	got := resolveCLIDebugDirWith("relconfig/events", stubGetenv("1"))
	if got == "" {
		t.Fatalf("relative event log: want enabled absolute dir, got disabled")
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("debug dir = %q, want absolute (must not resolve against CLI subprocess CWD)", got)
	}
	// The absolute root is anchored to the process CWD + relative parent,
	// and ends in the cli-debug leaf so capture still works.
	wantSuffix := filepath.Join("relconfig", "cli-debug")
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("debug dir = %q, want suffix %q", got, wantSuffix)
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
