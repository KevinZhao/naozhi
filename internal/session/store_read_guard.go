package session

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/naozhi/naozhi/internal/cli"
)

// jsonfile.Load's contract: a non-nil error means the file is still on disk and
// could not be read — over the size cap, an I/O error, not a regular file, a
// symlink, or the corrupt-rename itself failed. Its godoc spells out the
// consequence: "Callers MUST NOT continue with empty state in that case: the
// next atomic save would clobber the real file."
//
// The four session-store loaders did exactly that: slog.Warn + return nil, and
// the next WriteFileAtomic replaced the operator's sessions.json with a fresh,
// near-empty one (#2680). #469 fixed the same shape in cron by aborting startup;
// a chat service that refuses to boot over one oversized file is a worse trade,
// so this takes the other option from the issue — start, but refuse to write the
// files whose contents are unknown.
//
// State is keyed by absolute path rather than held on Router because the four
// loaders are package functions with one production call site each and 38 test
// call sites between them. Threading a receiver through would rewrite those
// tests without making the guard any harder to bypass, and test paths come from
// t.TempDir(), so two tests cannot collide on a key.
var storeReadBlocked sync.Map // path -> reason

// markStoreReadUnreadable records that path could not be read while still on
// disk, so writers must leave it alone. It is idempotent and emits the operator
// signal once per (path, reason) so `naozhi config check` surfaces it instead of
// the failure living only in a startup warning.
func markStoreReadUnreadable(path, label string, err error) {
	if path == "" || err == nil {
		return
	}
	reason := fmt.Sprintf("%s could not be read (%v); the file is still on disk and naozhi holds no copy of it", label, err)
	if prev, loaded := storeReadBlocked.LoadOrStore(path, reason); loaded && prev == reason {
		return
	}
	storeReadBlocked.Store(path, reason)
	slog.Error("session store: refusing to overwrite a file naozhi could not read",
		"path", path,
		"label", label,
		"err", err,
		"hint", "fix or move the file aside; until then changes to it are not persisted")
	cli.EmitSpawnDiags("config", []cli.SpawnDiag{{
		Layer:  "store-unreadable",
		Key:    label,
		Action: "ignored",
		Reason: reason,
	}})
}

// markStoreReadUnreadableReportOnly reports an unreadable file without blocking
// writes to it. Used for the meta sidecar: it is machine-written, a missing one
// reads as legacy, and blocking it would freeze the version number while
// sessions.json keeps advancing.
func markStoreReadUnreadableReportOnly(path, label string, err error) {
	if path == "" || err == nil {
		return
	}
	slog.Warn("session store: file could not be read",
		"path", path, "label", label, "err", err,
		"hint", "this file is regenerated on the next save; no operator data is at risk")
	cli.EmitSpawnDiags("config", []cli.SpawnDiag{{
		Layer:  "store-unreadable",
		Key:    label,
		Action: "ignored",
		Reason: fmt.Sprintf("%s could not be read (%v); it will be regenerated", label, err),
	}})
}

// clearStoreReadUnreadable lifts the block after a read that leaves naozhi in
// possession of the path: Parsed (we have the data), Absent (nothing there) and
// CorruptPreserved (the bad file was renamed away, so the path is now free) all
// qualify. Called on every successful load so an operator who fixes the file
// gets persistence back at the next read without restarting.
func clearStoreReadUnreadable(path string) {
	if path == "" {
		return
	}
	if _, had := storeReadBlocked.LoadAndDelete(path); had {
		slog.Info("session store: file is readable again; resuming saves", "path", path)
	}
}

// errStoreReadBlocked is returned by every writer whose target is blocked. It
// carries the reason so the caller's warning says why, and the callers already
// treat a save error as "keep the dirty flag set", which is the behaviour we
// want: the in-memory state stays authoritative and retries on the next tick.
type errStoreReadBlocked struct {
	path   string
	reason string
}

func (e *errStoreReadBlocked) Error() string {
	return "refusing to overwrite an unreadable session store file: " + e.reason
}

// blockedIfUnreadable returns a non-nil error when path must not be written.
// Every writer in store.go calls it first; that is the whole enforcement point.
func blockedIfUnreadable(path string) error {
	if path == "" {
		return nil
	}
	if v, ok := storeReadBlocked.Load(path); ok {
		reason, _ := v.(string)
		return &errStoreReadBlocked{path: path, reason: reason}
	}
	return nil
}
