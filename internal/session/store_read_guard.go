package session

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// jsonfile.Load's contract: a non-nil error means the file is still on disk and
// could not be read — over the size cap, an I/O error, not a regular file, a
// symlink, or the corrupt-rename itself failed. Its godoc spells out the
// consequence: "Callers MUST NOT continue with empty state in that case: the
// next atomic save would clobber the real file."
//
// The session-store loaders did exactly that: slog.Warn + return nil, and the
// next WriteFileAtomic replaced the operator's sessions.json with a fresh,
// near-empty one (#2680). #469 fixed the same shape in cron by aborting startup;
// a chat service that refuses to boot over one oversized file is a worse trade,
// so this takes the other option from the issue — start, but refuse to write the
// files whose contents are unknown, for the life of the process. The loaders run
// once, in NewRouter, so the fix is to repair or move the file aside and restart.
//
// State is keyed by absolute path rather than held on Router because the
// loaders are package functions with one production call site each and many
// test call sites. Test paths come from t.TempDir(), so two tests cannot
// collide on a key.
var storeReadBlocked sync.Map // path -> StoreBlock

// StoreBlock is the operator-facing view of one blocked store file, served on
// authenticated /health so "my sessions are not being persisted" is visible
// before a restart reveals it.
type StoreBlock struct {
	Path   string    `json:"path"`
	Label  string    `json:"label"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// markStoreReadUnreadable records that path could not be read while still on
// disk, so writers must leave it alone. It is idempotent and emits the operator
// signal (an Error log plus a store-unreadable spawn diag) once per path.
func markStoreReadUnreadable(path, label string, err error) {
	if path == "" || err == nil {
		return
	}
	reason := fmt.Sprintf("%s could not be read (%v); the file is still on disk and naozhi holds no copy of it", label, err)
	b := StoreBlock{Path: path, Label: label, Reason: reason, Since: time.Now()}
	if _, loaded := storeReadBlocked.LoadOrStore(path, b); loaded {
		return
	}
	slog.Error("session store: refusing to overwrite a file naozhi could not read",
		"path", path,
		"label", label,
		"err", err,
		"hint", "stop naozhi, fix or move the file aside (a symlink: replace it with the real file, or point session.store_path at the real location), then start naozhi; until then changes to it are not persisted")
	cli.EmitSpawnDiags("config", []cli.SpawnDiag{{
		Layer:  "store-unreadable",
		Key:    label,
		Action: "ignored",
		Reason: reason,
	}})
}

// clearStoreReadUnreadable lifts the block after a read that leaves naozhi in
// possession of the path: Parsed, Absent and CorruptPreserved all qualify.
func clearStoreReadUnreadable(path string) {
	storeReadBlocked.Delete(path)
}

// errStoreReadBlocked is returned by every writer whose target is blocked. The
// callers treat a save error as "keep the dirty flag set", so the in-memory
// state stays authoritative and retries on the next tick.
type errStoreReadBlocked struct {
	path   string
	reason string
}

func (e *errStoreReadBlocked) Error() string {
	return "refusing to overwrite an unreadable session store file: " + e.reason
}

// logStoreSaveFailure logs a save error: a blocked store was already reported
// at Error when it was blocked, so the per-tick repeat is Debug (#2972: at Warn
// the 30s tick produced ~2,880 identical lines a day); everything else warns.
func logStoreSaveFailure(msg string, err error) {
	var blocked *errStoreReadBlocked
	if errors.As(err, &blocked) {
		slog.Debug(msg, "err", err)
		return
	}
	slog.Warn(msg, "err", err)
}

// blockedIfUnreadable returns a non-nil error when path must not be written.
// Every writer in store.go calls it first; that is the whole enforcement point.
func blockedIfUnreadable(path string) error {
	if path == "" {
		return nil
	}
	v, ok := storeReadBlocked.Load(path)
	if !ok {
		return nil
	}
	return &errStoreReadBlocked{path: path, reason: v.(StoreBlock).Reason}
}

// StoreWriteBlocks is the Router-scoped view for /health: only the blocks on
// this Router's own store files, sorted by path, so a test harness with several
// Routers in one process does not see its neighbours'.
func (r *Router) StoreWriteBlocks() []StoreBlock {
	if r == nil || r.storePath == "" {
		return nil
	}
	var out []StoreBlock
	for _, p := range []string{r.storePath, knownIDsPath(r.storePath), workspaceOverridesPath(r.storePath)} {
		if v, ok := storeReadBlocked.Load(p); ok {
			out = append(out, v.(StoreBlock))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
