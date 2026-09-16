package config

// File-level migration: read the operator's config.yaml, run the chain over its
// yaml.Node document, and either report what would change or write it back.
//
// The write follows the same discipline AppendAccessProfile established: encode,
// RE-PARSE and re-validate the produced bytes, and only then replace the file
// atomically at 0600. A surgery bug must not be able to put a document on disk
// that the load path would refuse — that would turn a convenience command into
// an outage.

import (
	"bytes"
	"fmt"
	"os"

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
	applied, err := MigrateDocument(doc.Content[0])
	if err != nil {
		return res, err
	}
	res.Applied = applied
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

	// The produced document must load. Unmarshal + the full validator, not just
	// a YAML parse: a rewrite that dropped a required key would otherwise be
	// written and only fail at the next boot.
	var check Config
	if err := yaml.Unmarshal(buf.Bytes(), &check); err != nil {
		return res, fmt.Errorf("re-parse produced config: %w", err)
	}
	applyDefaults(&check)
	if err := parseDurations(&check); err != nil {
		return res, fmt.Errorf("produced config failed duration parsing: %w", err)
	}
	if err := validateConfig(&check); err != nil {
		return res, fmt.Errorf("produced config failed validation: %w", err)
	}
	res.After = buf.Bytes()
	return res, nil
}

// WriteMigrated replaces the config at path with res.After, atomically at 0600.
// It refuses when the result carries no change, so a caller cannot rewrite a
// file (and reformat it) for nothing.
func WriteMigrated(path string, res MigrateResult) error {
	if !res.Changed() {
		return fmt.Errorf("nothing to migrate")
	}
	if err := osutil.WriteFileAtomic(path, res.After, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
