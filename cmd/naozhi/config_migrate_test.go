package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The exit codes are the contract a CI check or an upgrade script depends on:
// 1 means "a migration is pending" (dry run), 0 means "nothing to do, or done",
// 2 means the file could not be handled at all. A dry run that exited 0 would
// make `config migrate` useless as a gate.
func TestConfigMigrate_ExitCodes(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return p
	}

	t.Run("dry run with a pending migration exits 1 and writes nothing", func(t *testing.T) {
		path := write(t, "schema_version: 1\nsession:\n  workspace: \"/home/u\"\n")
		before, _ := os.ReadFile(path)
		var out bytes.Buffer
		if code := configMigrate([]string{"-config", path}, &out); code != 1 {
			t.Fatalf("exit = %d, want 1; output:\n%s", code, out.String())
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Error("a dry run must not touch the file")
		}
		s := out.String()
		if !strings.Contains(s, "would write") || !strings.Contains(s, "dry run") {
			t.Errorf("dry run must show the document and say it is a dry run:\n%s", s)
		}
		// The operator has to be able to see WHAT changed, not just how many bytes.
		if !strings.Contains(s, "cwd:") {
			t.Errorf("the printed document must contain the migrated key:\n%s", s)
		}
	})

	t.Run("-write applies and is idempotent", func(t *testing.T) {
		path := write(t, "schema_version: 1\nsession:\n  workspace: \"/home/u\"\n")
		var out bytes.Buffer
		if code := configMigrate([]string{"-config", path, "-write"}, &out); code != 0 {
			t.Fatalf("exit = %d, want 0; output:\n%s", code, out.String())
		}
		got, _ := os.ReadFile(path)
		if !strings.Contains(string(got), "cwd:") || strings.Contains(string(got), "workspace:") {
			t.Errorf("the file must carry the migrated key only:\n%s", got)
		}
		if !strings.Contains(string(got), "schema_version: 2") {
			t.Errorf("the version must be bumped on disk:\n%s", got)
		}
		// Permissions matter: a config is refused by the loader when it is
		// group/world readable, so a migration that widened them would break
		// the next boot.
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("migrated file mode = %o, want 600", perm)
		}

		var out2 bytes.Buffer
		if code := configMigrate([]string{"-config", path}, &out2); code != 0 {
			t.Errorf("a migrated file must report nothing to do, exit = %d:\n%s", code, out2.String())
		}
	})

	t.Run("already current exits 0 without printing a document", func(t *testing.T) {
		path := write(t, "schema_version: 2\nsession:\n  cwd: \"/home/u\"\n")
		var out bytes.Buffer
		if code := configMigrate([]string{"-config", path}, &out); code != 0 {
			t.Fatalf("exit = %d, want 0; output:\n%s", code, out.String())
		}
		if strings.Contains(out.String(), "would write") {
			t.Errorf("nothing to do must not print a document:\n%s", out.String())
		}
	})

	t.Run("unreadable config exits 2", func(t *testing.T) {
		var out bytes.Buffer
		if code := configMigrate([]string{"-config", filepath.Join(t.TempDir(), "nope.yaml")}, &out); code != 2 {
			t.Fatalf("exit = %d, want 2; output:\n%s", code, out.String())
		}
	})

	t.Run("a conflict a human must resolve exits 2 and writes nothing", func(t *testing.T) {
		path := write(t, "schema_version: 1\nagents:\n  reviewer:\n    system_prompt: \"one\"\n    args: [\"--append-system-prompt\", \"other\"]\n")
		before, _ := os.ReadFile(path)
		var out bytes.Buffer
		if code := configMigrate([]string{"-config", path, "-write"}, &out); code != 2 {
			t.Fatalf("exit = %d, want 2; output:\n%s", code, out.String())
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Error("a refused migration must not have touched the file")
		}
	})
}
