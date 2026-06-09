package datadir

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPathConstructors(t *testing.T) {
	root := "/data/naozhi"
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"sessions", SessionsPath(root), "/data/naozhi/sessions.json"},
		{"events", EventsRoot(root), "/data/naozhi/events"},
		{"cron_jobs", CronJobsPath(root), "/data/naozhi/cron_jobs.json"},
		{"cron_runs", CronRunsRoot(root), "/data/naozhi/runs"},
		{"cli_debug", CLIDebugRoot(root), "/data/naozhi/cli-debug"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestPathConstructors_EmptyRoot(t *testing.T) {
	if SessionsPath("") != "" || EventsRoot("") != "" || CronJobsPath("") != "" || CronRunsRoot("") != "" || CLIDebugRoot("") != "" {
		t.Error("empty data root must yield empty paths")
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
