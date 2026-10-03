package config

// File-level migration: read the operator's config.yaml, run the chain over its
// yaml.Node document, and either report what would change or write it back.
//
// The write follows the same discipline AppendAccessProfile established: encode,
// RE-PARSE and re-validate the produced bytes, and only then replace the file
// atomically at 0600. A surgery bug must not be able to put a document on disk
// that the load path would refuse — that would turn a convenience command into
// an outage.
//
// The original bytes are kept next to the file before it is replaced: a
// migrated file declares the new schema_version, which an older binary refuses
// to load, so a downgrade needs the pre-migration file back.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/naozhi/naozhi/internal/osutil"
)

// MigrateResult is what a migration run did, or would do.
type MigrateResult struct {
	// Applied describes each migration that changed something, plus the
	// schema_version bump. Empty means the file is already current.
	Applied []string
	// Before and After are the file bytes; equal when nothing changed.
	Before, After []byte
	// From is the schema_version the file declared (absent = unversionedSchema).
	From int
	// Warnings are validation failures the produced document shares with the
	// original, so the migration did not cause them. The usual one is an
	// unset ${VAR}: `config migrate` runs in an operator's shell, which rarely
	// has the systemd EnvironmentFile loaded, and the placeholder is what the
	// file is supposed to carry. Reported, not fatal.
	Warnings []string
}

// Changed reports whether the migration would rewrite the file.
func (r MigrateResult) Changed() bool { return !bytes.Equal(r.Before, r.After) }

// MigrateFile runs the chain against the config at path. It never writes; the
// caller decides, so `config migrate` can show the result and `--write` can
// commit the same bytes it showed.
func MigrateFile(path string) (MigrateResult, error) {
	var res MigrateResult
	data, err := os.ReadFile(path)
	if err != nil {
		return res, fmt.Errorf("read config: %w", err)
	}
	res.Before = data
	res.After = data

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return res, fmt.Errorf("parse config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return res, fmt.Errorf("config is not a valid YAML document")
	}
	// Read before MigrateDocument bumps it; a malformed value is reported by
	// MigrateDocument itself.
	from, _ := documentSchemaVersion(doc.Content[0])
	applied, err := MigrateDocument(doc.Content[0])
	if err != nil {
		return res, err
	}
	res.Applied = applied
	res.From = from
	if len(applied) == 0 {
		return res, nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return res, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return res, fmt.Errorf("close encoder: %w", err)
	}

	// The produced document must load: the full validator, not just a YAML
	// parse, or a rewrite that dropped a required key would only fail at the
	// next boot. Validation sees the bytes the way Load does — after ${VAR}
	// expansion — while the file keeps the placeholders (#2968).
	if err := checkProduced(buf.Bytes()); err != nil {
		// A failure the original document has too was not caused by this
		// migration (typically an operator shell without the service's env);
		// it is reported, not fatal. A failure only the produced document has
		// is the surgery's own, and still refuses.
		if before := checkProduced(data); before != nil && before.Error() == err.Error() {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("config fails validation before and after migration (not caused by it): %s", err))
		} else {
			return res, err
		}
	}
	res.After = buf.Bytes()
	return res, nil
}

// checkProduced runs the load path's checks over raw config bytes: env
// expansion, decode, defaults, durations, validation. It is the migration's
// re-validate step, so the wording of each error names the produced config.
func checkProduced(raw []byte) error {
	var check Config
	if err := yaml.Unmarshal(expandEnvVars(raw), &check); err != nil {
		return fmt.Errorf("re-parse produced config: %w", err)
	}
	applyDefaults(&check)
	if err := parseDurations(&check); err != nil {
		return fmt.Errorf("produced config failed duration parsing: %w", err)
	}
	if err := validateConfig(&check); err != nil {
		return fmt.Errorf("produced config failed validation: %w", err)
	}
	return nil
}

// WriteMigrated replaces the config at path with res.After, atomically at 0600,
// after keeping res.Before as a backup it returns the path of. It refuses when
// the result carries no change, so a caller cannot rewrite a file (and reformat
// it) for nothing, and when the file no longer holds res.Before, so the backup
// is what the dry run diffed against and a concurrent edit is not overwritten.
func WriteMigrated(path string, res MigrateResult) (backup string, err error) {
	if !res.Changed() {
		return "", fmt.Errorf("nothing to migrate")
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("re-read config: %w", err)
	}
	if !bytes.Equal(cur, res.Before) {
		return "", fmt.Errorf("config changed since it was read; re-run config migrate")
	}
	backup, err = writeMigrateBackup(path, res.Before, res.From, time.Now())
	if err != nil {
		return "", fmt.Errorf("back up config (file left unchanged): %w", err)
	}
	if err := osutil.WriteFileAtomic(path, res.After, 0o600); err != nil {
		return backup, fmt.Errorf("write config: %w", err)
	}
	return backup, nil
}

// maxBackupNames bounds the timestamped names writeMigrateBackup tries.
const maxBackupNames = 100

// writeMigrateBackup durably writes data to <path>.pre-migrate-v<from> at 0600
// and returns the name used. The name carries no .yaml suffix, so nothing takes
// it for a live config. An existing backup is never overwritten: one holding
// the same bytes is reused, otherwise a UTC-timestamped name is taken.
func writeMigrateBackup(path string, data []byte, from int, now time.Time) (string, error) {
	base := migrateBackupBase(path, from)
	stamped := base + "." + now.UTC().Format("20060102T150405Z")
	for i := 0; i < maxBackupNames; i++ {
		name := base
		switch {
		case i == 1:
			name = stamped
		case i > 1:
			name = fmt.Sprintf("%s-%d", stamped, i)
		}
		err := createBackup(name, data)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		if sameRegularFile(name, data) {
			return name, nil
		}
	}
	return "", fmt.Errorf("no free backup name after %s (%d tried)", base, maxBackupNames)
}

func migrateBackupBase(path string, from int) string {
	return fmt.Sprintf("%s.pre-migrate-v%d", path, from)
}

// MigrateBackupName is the backup name WriteMigrated tries first for res.
// taken reports that the name already holds other bytes, so the write will
// keep the original under a timestamped name next to it instead.
func MigrateBackupName(path string, res MigrateResult) (name string, taken bool) {
	name = migrateBackupBase(path, res.From)
	if _, err := os.Lstat(name); err != nil {
		return name, false
	}
	return name, !sameRegularFile(name, res.Before)
}

// backupSync and backupSyncDir indirect the fsyncs so tests can inject failures.
var (
	backupSync    = (*os.File).Sync
	backupSyncDir = osutil.SyncDir
)

// createBackup writes data to a new file at name (O_EXCL, 0600), fsyncs it and
// its directory. A failed write removes the file it created. A failed directory
// fsync only degrades the entry's crash durability (Windows cannot fsync a
// directory at all), so it is logged, as WriteFileAtomic does.
func createBackup(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = backupSync(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := backupSyncDir(filepath.Dir(name)); err != nil {
		slog.Warn("config migrate: backup written, directory fsync failed", "path", name, "err", err)
	}
	return nil
}

// sameRegularFile reports whether name is a regular file (not a symlink)
// holding exactly data.
func sameRegularFile(name string, data []byte) bool {
	fi, err := os.Lstat(name)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	got, err := os.ReadFile(name)
	return err == nil && bytes.Equal(got, data)
}
