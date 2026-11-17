package project

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A project.yaml that came in with a clone is read bounded: git keeps
// symlinks, so it can point at /dev/zero (os.ReadFile grows until the
// process runs out of memory) or at a FIFO (the read blocks the scan for
// good). Each case must fail at once as a bad config.
func TestLoadConfig_RefusesUnboundedFiles(t *testing.T) {
	t.Parallel()
	cases := map[string]func(t *testing.T, cfgPath string){
		"oversized": func(t *testing.T, cfgPath string) {
			big := "planner_model: sonnet\n" + strings.Repeat("# padding\n", maxConfigBytes/10+1)
			if err := os.WriteFile(cfgPath, []byte(big), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink to fifo": func(t *testing.T, cfgPath string) {
			fifo := filepath.Join(t.TempDir(), "fifo")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			if err := os.Symlink(fifo, cfgPath); err != nil {
				t.Fatal(err)
			}
		},
		"symlink to oversized": func(t *testing.T, cfgPath string) {
			big := filepath.Join(t.TempDir(), "big.yaml")
			if err := os.WriteFile(big, []byte(strings.Repeat("# padding\n", maxConfigBytes/10+1)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(big, cfgPath); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, cfgPath string) {
			if err := syscall.Mkfifo(cfgPath, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			proj := t.TempDir()
			if err := os.MkdirAll(filepath.Join(proj, configDir), 0o700); err != nil {
				t.Fatal(err)
			}
			setup(t, filepath.Join(proj, configDir, configFile))

			done := make(chan error, 1)
			go func() {
				_, err := loadConfig(proj)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("loadConfig accepted the file, want a bad-config error")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("loadConfig still blocked after 3s")
			}
		})
	}
}

// A project.yaml kept as a symlink to an ordinary file elsewhere (a shared
// or dotfiles directory) still loads: refusing the link would make Scan drop
// the whole project, its sidebar entry and chat bindings with it.
func TestLoadConfig_FollowsSymlinkToRegularFile(t *testing.T) {
	t.Parallel()
	shared := filepath.Join(t.TempDir(), "project.yaml")
	if err := os.WriteFile(shared, []byte("planner_model: sonnet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, configDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(proj, configDir, configFile)); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(proj)
	if err != nil {
		t.Fatalf("loadConfig through a symlink to a regular file: %v", err)
	}
	if cfg.PlannerModel != "sonnet" {
		t.Errorf("PlannerModel = %q, want sonnet read through the link", cfg.PlannerModel)
	}
}
