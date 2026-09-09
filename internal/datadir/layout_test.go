package datadir

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLayoutSiblings(t *testing.T) {
	lay := ForStore("/data/naozhi/sessions.json")
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"root", lay.Root(), "/data/naozhi"},
		{"events", lay.EventsRoot(), "/data/naozhi/events"},
		{"cost", lay.CostRoot(), "/data/naozhi/cost"},
		{"session_runs", lay.SessionRunsRoot(), "/data/naozhi/session-runs"},
		{"session_ids", lay.SessionIDsPath(), "/data/naozhi/session-ids.json"},
		{"workspace_overrides", lay.WorkspaceOverridesPath(), "/data/naozhi/workspace-overrides.json"},
		{"cli_debug", lay.CLIDebugRoot(), "/data/naozhi/cli-debug"},
		{"access_profile_secrets", lay.AccessProfileSecretsRoot(), "/data/naozhi/access-profile-secrets"},
		{"sys_sessions", lay.SysSessionsRoot(), "/data/naozhi/sys-sessions"},
		{"naozhi_settings", lay.NaozhiSettingsPath(), "/data/naozhi/naozhi-settings.json"},
		{"ui_settings", lay.UISettingsPath(), "/data/naozhi/ui-settings.json"},
		{"cron_runs", lay.RunsRoot(), "/data/naozhi/runs"},
		{"join", lay.Join("a", "b"), "/data/naozhi/a/b"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestLayoutNeverRebuildsTheStorePath is why the previous API had zero callers:
// SessionsPath(root) rebuilt "<root>/sessions.json", so an operator who
// configured any other filename would have had that configuration silently
// discarded. Layout derives siblings only — there is no method returning the
// store file, because the caller already has it.
func TestLayoutNeverRebuildsTheStorePath(t *testing.T) {
	lay := ForStore("/data/naozhi/mystore.json")
	if got := lay.Root(); got != "/data/naozhi" {
		t.Fatalf("Root() = %q, want /data/naozhi", got)
	}
	for _, p := range []string{
		lay.EventsRoot(), lay.CostRoot(), lay.SessionIDsPath(), lay.RunsRoot(),
		lay.SessionRunsRoot(), lay.UISettingsPath(), lay.NaozhiSettingsPath(),
	} {
		if base := filepath.Base(p); base == "sessions.json" || base == "cron_jobs.json" {
			t.Errorf("Layout produced a configured store filename (%q); it must only name siblings", p)
		}
	}
}

// TestZeroLayoutYieldsEmptyPaths: a Layout built from an unset store path must
// not turn into writes at the filesystem root. Every open-coded site it replaced
// degraded quietly the same way.
func TestZeroLayoutYieldsEmptyPaths(t *testing.T) {
	for name, lay := range map[string]Layout{"zero": {}, "for_empty_store": ForStore(""), "from_empty_root": FromRoot("")} {
		for _, p := range []string{
			lay.Root(), lay.Join("x"), lay.EventsRoot(), lay.CostRoot(), lay.RunsRoot(),
			lay.SessionIDsPath(), lay.WorkspaceOverridesPath(), lay.SessionRunsRoot(),
			lay.CLIDebugRoot(), lay.AccessProfileSecretsRoot(), lay.SysSessionsRoot(),
			lay.NaozhiSettingsPath(), lay.UISettingsPath(),
		} {
			if p != "" {
				t.Errorf("%s: got %q, want empty", name, p)
			}
		}
	}
}

func TestEnsureDir_CreatesAt0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "leaf")
	if err := EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir() error = %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat after EnsureDir: %v", err)
	}
	if !fi.IsDir() {
		t.Fatal("EnsureDir did not create a directory")
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != DirMode {
			t.Errorf("mode = %s, want %s", perm, DirMode)
		}
	}
}

func TestEnsureDir_TightensLooseMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix perms only")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "loose")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir() error = %v", err)
	}
	fi, _ := os.Stat(dir)
	if perm := fi.Mode().Perm(); perm != DirMode {
		t.Errorf("pre-existing 0755 dir not tightened: mode = %s, want %s", perm, DirMode)
	}
}

func TestEnsureDir_RejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(link); err == nil {
		t.Error("EnsureDir on a symlink must error (redirect guard)")
	}
}

func TestEnsureDir_EmptyPathNoop(t *testing.T) {
	if err := EnsureDir(""); err != nil {
		t.Errorf("EnsureDir(\"\") = %v, want nil", err)
	}
}
