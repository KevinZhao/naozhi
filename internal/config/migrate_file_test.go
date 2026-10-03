package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const migrateV1Config = "session:\n  workspace: \"/home/u\"\n"

func writeMigrateFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// A migrated file declares a schema_version an older binary refuses, so the
// original has to survive the write: a private copy named for the version it
// came from, holding exactly the bytes the dry run diffed against.
func TestWriteMigrated_KeepsTheOriginalAsABackup(t *testing.T) {
	path := writeMigrateFixture(t, migrateV1Config)
	res, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	if res.From != 1 {
		t.Errorf("From = %d, want 1 for a file without schema_version", res.From)
	}
	backup, err := WriteMigrated(path, res)
	if err != nil {
		t.Fatalf("WriteMigrated: %v", err)
	}
	if want := path + ".pre-migrate-v1"; backup != want {
		t.Errorf("backup = %q, want %q", backup, want)
	}
	if got := readFile(t, backup); !bytes.Equal(got, res.Before) {
		t.Errorf("backup holds %q, want the original %q", got, res.Before)
	}
	if got := readFile(t, path); !bytes.Equal(got, res.After) {
		t.Errorf("config holds %q, want the migrated %q", got, res.After)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(backup)
		if err != nil {
			t.Fatalf("stat backup: %v", err)
		}
		// The config carries secrets.
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("backup mode = %o, want 600", perm)
		}
	}
}

// The name records the version the file declared, not a fixed v1: a file that
// declares v2 but still carries a v1 key is repaired too.
func TestMigrateFile_FromIsTheDeclaredVersion(t *testing.T) {
	path := writeMigrateFixture(t, "schema_version: 2\n"+migrateV1Config)
	res, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	if res.From != 2 {
		t.Errorf("From = %d, want 2", res.From)
	}
	backup, err := WriteMigrated(path, res)
	if err != nil {
		t.Fatalf("WriteMigrated: %v", err)
	}
	if !strings.HasSuffix(backup, ".pre-migrate-v2") {
		t.Errorf("backup = %q, want the .pre-migrate-v2 name", backup)
	}
}

// An older backup is somebody's only copy of an earlier original: a different
// one is never overwritten, an identical one is reused instead of piling up.
func TestWriteMigrated_NeverOverwritesAnOlderBackup(t *testing.T) {
	t.Run("different bytes take a timestamped name", func(t *testing.T) {
		path := writeMigrateFixture(t, migrateV1Config)
		older := []byte("# an earlier original\n")
		if err := os.WriteFile(path+".pre-migrate-v1", older, 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := MigrateFile(path)
		if err != nil {
			t.Fatalf("MigrateFile: %v", err)
		}
		backup, err := WriteMigrated(path, res)
		if err != nil {
			t.Fatalf("WriteMigrated: %v", err)
		}
		if !strings.HasPrefix(backup, path+".pre-migrate-v1.") || !strings.HasSuffix(backup, "Z") {
			t.Errorf("backup = %q, want <path>.pre-migrate-v1.<UTC stamp>", backup)
		}
		if got := readFile(t, path+".pre-migrate-v1"); !bytes.Equal(got, older) {
			t.Errorf("the older backup was overwritten: %q", got)
		}
		if got := readFile(t, backup); !bytes.Equal(got, res.Before) {
			t.Errorf("new backup holds %q, want %q", got, res.Before)
		}
	})

	t.Run("identical bytes are reused", func(t *testing.T) {
		path := writeMigrateFixture(t, migrateV1Config)
		if err := os.WriteFile(path+".pre-migrate-v1", []byte(migrateV1Config), 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := MigrateFile(path)
		if err != nil {
			t.Fatalf("MigrateFile: %v", err)
		}
		backup, err := WriteMigrated(path, res)
		if err != nil {
			t.Fatalf("WriteMigrated: %v", err)
		}
		if backup != path+".pre-migrate-v1" {
			t.Errorf("backup = %q, want the existing identical one reused", backup)
		}
		if names := dirNames(t, filepath.Dir(path)); len(names) != 2 {
			t.Errorf("dir holds %q, want only the config and one backup", names)
		}
	})
}

// The fallback names are deterministic for a given clock, collisions within the
// same second count up, and a symlink is never taken for an identical backup.
func TestWriteMigrateBackup_Names(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte("original\n")
	now := time.Date(2026, 10, 4, 1, 2, 3, 0, time.FixedZone("x", 8*3600))
	base := path + ".pre-migrate-v1"
	stamped := base + ".20261003T170203Z"

	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stamped, []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := writeMigrateBackup(path, data, 1, now)
	if err != nil {
		t.Fatalf("writeMigrateBackup: %v", err)
	}
	if want := stamped + "-2"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if b := readFile(t, got); !bytes.Equal(b, data) {
		t.Errorf("backup holds %q, want %q", b, data)
	}
	if b := readFile(t, stamped); string(b) != "other\n" {
		t.Errorf("an existing stamped backup was overwritten: %q", b)
	}
}

// WriteMigrated commits what MigrateFile read. A file edited in between would
// be overwritten with a migration of its old content, and the backup would not
// be the file the operator had; refuse, and touch nothing.
func TestWriteMigrated_RefusesAFileChangedSinceItWasRead(t *testing.T) {
	path := writeMigrateFixture(t, migrateV1Config)
	res, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	edited := []byte(migrateV1Config + "# edited meanwhile\n")
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := WriteMigrated(path, res)
	if err == nil || !strings.Contains(err.Error(), "changed since it was read") {
		t.Fatalf("err = %v, want a changed-since-read refusal", err)
	}
	if backup != "" {
		t.Errorf("backup = %q, want none", backup)
	}
	if got := readFile(t, path); !bytes.Equal(got, edited) {
		t.Errorf("config was rewritten: %q", got)
	}
	if names := dirNames(t, filepath.Dir(path)); !slices.Equal(names, []string{"config.yaml"}) {
		t.Errorf("dir holds %q, want the config alone", names)
	}
}

// No backup, no rewrite: the config must not be replaced when its original
// could not be kept.
func TestWriteMigrated_BackupFailureLeavesTheConfigAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not restrict file creation on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	path := writeMigrateFixture(t, migrateV1Config)
	res, err := MigrateFile(path)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err = WriteMigrated(path, res)
	if err == nil || !strings.Contains(err.Error(), "back up config") {
		t.Fatalf("err = %v, want a backup failure", err)
	}
	if got := readFile(t, path); !bytes.Equal(got, res.Before) {
		t.Errorf("config was rewritten without a backup: %q", got)
	}
}
