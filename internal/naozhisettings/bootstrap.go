// Package naozhisettings manages the naozhi-owned Claude settings file: a
// settings.json deliberately ISOLATED from the operator's ~/.claude/settings.json
// so naozhi can be configured differently and a broken local file cannot take
// naozhi down. It is seeded ONCE from the local settings, then decoupled; cc is
// pointed at it via `--setting-sources "" --settings <file>`
// (cli.SpawnOptions.SettingsFile). Opt-in (RFC naozhi-owned-settings-v3).
package naozhisettings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/naozhi/naozhi/internal/osutil"
)

// strippedKeys are the top-level settings keys removed during bootstrap:
//   - "hooks": local hooks may call back into naozhi and dead-loop; the
//     isolated file carries no host hooks.
//   - "env": auth/upstream selection belongs to the audited access-profile
//     env overlay, and copying the block would duplicate any plaintext token
//     on disk. The settings file carries behaviour config only.
var strippedKeys = []string{"hooks", "env"}

// Bootstrap builds the naozhi-owned settings document from raw local
// settings.json bytes: drops strippedKeys and re-serialises every other key
// VERBATIM via json.RawMessage, so naozhi never needs the full schema.
//
// FAIL-SOFT: if local is not a valid JSON object, it returns "{}" and ok=false
// so the caller can warn and still write a usable file; err is never non-nil
// in practice.
func Bootstrap(local []byte) (doc []byte, ok bool, err error) {
	keys, vals, parseErr := parseTopLevelOrdered(local)
	if parseErr != nil {
		return []byte("{}"), false, nil
	}
	stripped := map[string]bool{}
	for _, k := range strippedKeys {
		stripped[k] = true
	}
	// Re-emit in original key order so the naozhi file is a minimal diff against
	// the local one (the RFC §5.3 "view differences" feature depends on it).
	return emitOrdered(keys, vals, stripped), true, nil
}

// SetTopLevel returns doc with every key in updates replaced IN PLACE, so key
// order and untouched values survive verbatim; keys absent from doc are appended
// in sorted order. A nil update value DELETES the key instead. An empty doc
// starts from "{}".
//
// Callers own only the keys they pass: this is how a settings file can be
// machine-maintained for a few keys while staying hand-editable everywhere else.
func SetTopLevel(doc []byte, updates map[string]json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(doc)) == 0 {
		doc = []byte("{}")
	}
	keys, vals, err := parseTopLevelOrdered(doc)
	if err != nil {
		return nil, err
	}
	pending := make(map[string]bool, len(updates))
	for k, v := range updates {
		pending[k] = v != nil
	}
	drop := map[string]bool{}
	for i, k := range keys {
		v, ok := updates[k]
		if !ok {
			continue
		}
		delete(pending, k)
		if v == nil {
			drop[k] = true
			continue
		}
		vals[i] = v
	}
	for _, k := range sortedKeys(pending) {
		keys = append(keys, k)
		vals = append(vals, updates[k])
	}
	return emitOrdered(keys, vals, drop), nil
}

// sortedKeys returns the true-valued keys of set in sorted order, so an appended
// key block is deterministic across runs.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k, v := range set {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// emitOrdered serialises parallel key/value slices as a two-space-indented JSON
// object, skipping any key in skip.
func emitOrdered(keys []string, vals []json.RawMessage, skip map[string]bool) []byte {
	var buf bytes.Buffer
	buf.WriteString("{")
	first := true
	for i, k := range keys {
		if skip[k] {
			continue
		}
		if !first {
			buf.WriteString(",")
		}
		first = false
		buf.WriteString("\n  ")
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteString(": ")
		buf.Write(indentValue(vals[i]))
	}
	if !first {
		buf.WriteString("\n")
	}
	buf.WriteString("}")
	return buf.Bytes()
}

// parseTopLevelOrdered parses a JSON object into parallel key/value slices in
// document order. Returns an error if data is empty or not a JSON object.
func parseTopLevelOrdered(data []byte) (keys []string, vals []json.RawMessage, err error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil, fmt.Errorf("empty document")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("top-level value is not an object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, nil, fmt.Errorf("non-string object key")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, err
		}
		keys = append(keys, key)
		vals = append(vals, raw)
	}
	return keys, vals, nil
}

// indentValue re-indents raw to sit under a two-space key; compact on error.
func indentValue(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "  ", "  "); err != nil {
		return raw
	}
	return buf.Bytes()
}

// EnsureBootstrapped writes the naozhi-owned settings file to path only if it
// does not exist, seeding from localSettingsPath; an existing file is left
// UNTOUCHED (existed=true) so operator tuning survives. Written 0600
// atomically. A missing/unreadable local file is not fatal: the file is still
// written and a non-nil err is advisory. Only a write/mkdir failure leaves no file.
func EnsureBootstrapped(path, localSettingsPath string) (existed, seededFromLocal bool, err error) {
	if _, statErr := os.Stat(path); statErr == nil {
		return true, false, nil
	} else if !os.IsNotExist(statErr) {
		return false, false, fmt.Errorf("stat naozhi settings: %w", statErr)
	}
	doc, seededFromLocal, diagErr := bootstrapFromFile(localSettingsPath)
	if writeErr := writeDoc(path, doc); writeErr != nil {
		return false, false, writeErr
	}
	// File written; diagErr (if any) is advisory only.
	return false, seededFromLocal, diagErr
}

// ReBootstrap unconditionally re-seeds path from localSettingsPath, discarding
// naozhi's current isolated config; MUST be gated behind an explicit operator
// confirmation. Error semantics match EnsureBootstrapped.
func ReBootstrap(path, localSettingsPath string) (seededFromLocal bool, err error) {
	doc, seededFromLocal, diagErr := bootstrapFromFile(localSettingsPath)
	if writeErr := writeDoc(path, doc); writeErr != nil {
		return false, writeErr
	}
	return seededFromLocal, diagErr
}

// writeDoc creates the parent dir (0700) if needed and atomically writes doc at
// 0600, so a first-run host without a data dir still gets a settings file.
func writeDoc(path string, doc []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create naozhi settings dir: %w", err)
		}
	}
	if err := osutil.WriteFileAtomic(path, doc, 0o600); err != nil {
		return fmt.Errorf("write naozhi settings: %w", err)
	}
	return nil
}

// bootstrapFromFile reads localSettingsPath (missing file tolerated) and runs
// Bootstrap. A present-but-unreadable file seeds empty and returns readErr so
// the caller can distinguish "absent" from "unreadable".
func bootstrapFromFile(localSettingsPath string) (doc []byte, seededFromLocal bool, err error) {
	local, readErr := os.ReadFile(localSettingsPath)
	if readErr != nil {
		local = nil
		if !os.IsNotExist(readErr) {
			doc, _, _ = Bootstrap(nil)
			return doc, false, fmt.Errorf("read local settings %q: %w", localSettingsPath, readErr)
		}
	}
	doc, seededFromLocal, _ = Bootstrap(local)
	return doc, seededFromLocal, nil
}
