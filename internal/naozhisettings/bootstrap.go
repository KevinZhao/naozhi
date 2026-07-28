// Package naozhisettings manages the naozhi-owned Claude settings file (RFC
// naozhi-owned-settings-v3): a settings.json that is deliberately ISOLATED from
// the operator's ~/.claude/settings.json.
//
// Today naozhi-spawned cc reads ~/.claude/settings.json directly via
// `--setting-sources user` (docs/rfc/direct-user-settings.md). That means
// naozhi cannot be configured differently from the operator's interactive cc
// (e.g. a deeper thinking budget for background sessions), and a broken local
// settings file takes naozhi's next session down with it.
//
// This package produces a file naozhi owns. It is seeded ONCE from the local
// settings (bootstrap), then decoupled: later edits to ~/.claude/settings.json
// do not flow in. cc is pointed at it via `--setting-sources "" --settings
// <file>` (see cli.SpawnOptions.SettingsFile). Opt-in: absent the file / with
// the feature off, naozhi keeps the legacy `--setting-sources user` path.
package naozhisettings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/osutil"
)

// strippedKeys are the top-level settings keys removed during bootstrap.
//
//   - "hooks": the local settings' hooks may call back into naozhi and
//     dead-loop (this is the very reason direct-user-settings' predecessor ran
//     `--setting-sources ""`). naozhi's isolated file is a clean execution
//     config with no host hooks. A high-level operator who hand-edits the file
//     to add hooks does so at their own risk.
//   - "env": auth/upstream selection is naozhi's job via the access-profile env
//     overlay (a separate, audited channel), NOT this settings file. Copying the
//     local env block would also drag any plaintext token in it onto disk in a
//     second place. Drop it entirely; the settings file carries behaviour config
//     only. (RFC naozhi-owned-settings-v3 §9 decision 2.)
var strippedKeys = []string{"hooks", "env"}

// Bootstrap builds the naozhi-owned settings document from the raw bytes of a
// local ~/.claude/settings.json. It parses the document as a generic top-level
// object, drops the stripped keys (hooks, env), and re-serialises the rest
// VERBATIM — every other key (mcpServers, permissions, model pins, feature
// toggles, …) is preserved byte-for-byte via json.RawMessage so naozhi never
// has to know the full settings schema.
//
// FAIL-SOFT: if local is not a valid JSON object (missing file → caller passes
// nil/empty; corrupt → parse error), Bootstrap returns a minimal empty document
// ("{}") and ok=false, so the caller can log a warning and still write a usable
// isolated file rather than aborting startup. A nil error is only returned for
// a genuine marshal failure (unreachable in practice). ok=true means the local
// document was well-formed and its non-stripped keys were carried over.
func Bootstrap(local []byte) (doc []byte, ok bool, err error) {
	keys, vals, parseErr := parseTopLevelOrdered(local)
	if parseErr != nil {
		// Missing or non-object local settings → seed an empty document. Not an
		// error: a first-run host may have no ~/.claude/settings.json at all.
		return []byte("{}"), false, nil
	}
	stripped := map[string]bool{}
	for _, k := range strippedKeys {
		stripped[k] = true
	}
	// Re-emit in the ORIGINAL key order (parseTopLevelOrdered preserves it) so
	// the naozhi file is a minimal, low-noise diff against the local one — the
	// RFC §5.3 "view differences" feature depends on stable ordering. A plain
	// map round-trip would reorder every key alphabetically.
	var buf bytes.Buffer
	buf.WriteString("{")
	first := true
	for i, k := range keys {
		if stripped[k] {
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
	return buf.Bytes(), true, nil
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

// indentValue re-indents a raw JSON value to sit under a two-space top-level
// key. Falls back to the compact form on any error (still valid JSON).
func indentValue(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "  ", "  "); err != nil {
		return raw
	}
	return buf.Bytes()
}

// EnsureBootstrapped writes the naozhi-owned settings file to path IF it does
// not already exist, seeding it from localSettingsPath. An already-present file
// is left UNTOUCHED (returns existed=true) — bootstrap is a one-time seed, never
// a silent overwrite of a file the operator has since tuned (thinking budget,
// hand edits). Use ReBootstrap for the explicit "re-init from local" action.
//
// The file is written 0600 (may carry mcp tokens or other config) via an atomic
// write. A missing / unreadable / corrupt local file is NOT fatal: the naozhi
// file is still written (seeded empty) and seededFromLocal=false; a non-nil err
// in that case is a DIAGNOSTIC (e.g. local file present but unreadable) — the
// file was still written, so the caller should warn but treat naozhi as usable.
// Only a genuine write/mkdir failure returns err WITHOUT a written file.
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

// ReBootstrap unconditionally re-seeds path from localSettingsPath, overwriting
// any existing naozhi-owned file. This is the "用本地重新初始化" action (RFC §5.4)
// and MUST be gated behind an explicit, confirmed operator request — it discards
// naozhi's current isolated config (including a tuned thinking budget). Like
// EnsureBootstrapped, a non-nil err after a successful write is advisory (local
// unreadable); a write/mkdir failure returns err without overwriting.
func ReBootstrap(path, localSettingsPath string) (seededFromLocal bool, err error) {
	doc, seededFromLocal, diagErr := bootstrapFromFile(localSettingsPath)
	if writeErr := writeDoc(path, doc); writeErr != nil {
		return false, writeErr
	}
	return seededFromLocal, diagErr
}

// writeDoc creates the destination's parent directory (0700) if needed and
// atomically writes doc at 0600. Owning the MkdirAll here (rather than pushing
// it onto every caller) keeps the package self-contained: a first-run host
// whose data dir does not yet exist still gets a usable naozhi settings file.
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

// bootstrapFromFile reads localSettingsPath (tolerating a missing file) and runs
// Bootstrap over its bytes. A present-but-unreadable local file (permission,
// I/O) is non-fatal — it falls through to an empty seed so naozhi still gets a
// usable isolated file — but returns readErr so the caller can distinguish
// "local absent" from "local unreadable" in its log (F5).
func bootstrapFromFile(localSettingsPath string) (doc []byte, seededFromLocal bool, err error) {
	local, readErr := os.ReadFile(localSettingsPath)
	if readErr != nil {
		local = nil
		if !os.IsNotExist(readErr) {
			// Seed empty but tell the caller the local file existed and could
			// not be read, so "seeded_from_local=false" is not mistaken for
			// "local had no settings".
			doc, _, _ = Bootstrap(nil)
			return doc, false, fmt.Errorf("read local settings %q: %w", localSettingsPath, readErr)
		}
	}
	doc, seededFromLocal, _ = Bootstrap(local)
	return doc, seededFromLocal, nil
}
