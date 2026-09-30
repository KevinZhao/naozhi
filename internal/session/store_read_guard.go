package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
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
// Lifting the block (#2972). The loaders run once, in NewRouter, so no later
// load can clear a block set at startup; every blocked write re-probes the
// path instead:
//
//   - gone, or truncated to 0 bytes — the operator moved it aside, which is what
//     the hint asks for. Nothing left to clobber; the block lifts and the write
//     proceeds.
//   - readable again with content — repaired in place, or the I/O fault passed.
//     Its contents were never loaded, so writing would still replace them with
//     the state of a process that started empty. The block stays, and the reason
//     changes to say a restart (or moving the file aside) is what lifts it.
//   - still unreadable — nothing changes.
//
// The probe is one Lstat per blocked write; the read happens only when size or
// mtime moved since the last probe.
//
// State is keyed by absolute path rather than held on Router because the four
// loaders are package functions with one production call site each and 38 test
// call sites between them. Threading a receiver through would rewrite those
// tests without making the guard any harder to bypass, and test paths come from
// t.TempDir(), so two tests cannot collide on a key.
var storeReadBlocked sync.Map // path -> *storeBlock

// storeBlock is one blocked path's state. Reads and writes of the mutable
// fields go through mu; the sync.Map only hands out the pointer.
type storeBlock struct {
	mu     sync.Mutex
	label  string
	reason string
	since  time.Time
	// probeSize / probeMod are the file as last probed, so a blocked write
	// tick re-reads only when the file changed.
	probeSize   int64
	probeMod    time.Time
	probed      bool
	needRestart bool
	// lastWarn throttles the per-tick "save failed" warning: the first one is
	// immediate, the rest one per storeBlockedWarnEvery.
	lastWarn time.Time
}

// storeBlockedWarnEvery is how often a blocked save repeats its warning. The
// first failure warns at once; the 30s tick then produced ~2,880 identical
// lines a day, which buried the one that mattered.
const storeBlockedWarnEvery = time.Hour

// StoreBlock is the operator-facing view of one blocked store file, served on
// authenticated /health so "my sessions are not being persisted" is visible
// before a restart reveals it.
type StoreBlock struct {
	Path   string    `json:"path"`
	Label  string    `json:"label"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
	// NeedRestart is set once the file has been seen readable again: its
	// contents were not loaded, so a restart (or moving it aside) is the fix.
	NeedRestart bool `json:"need_restart"`
}

// markStoreReadUnreadable records that path could not be read while still on
// disk, so writers must leave it alone. It is idempotent and emits the operator
// signal once per (path, reason): the diag lands in the process-wide spawndiag
// summary that authenticated /health serves as spawn_diags, and the live block
// list is served there as session_store.blocked.
func markStoreReadUnreadable(path, label string, err error) {
	if path == "" || err == nil {
		return
	}
	reason := fmt.Sprintf("%s could not be read (%v); the file is still on disk and naozhi holds no copy of it", label, err)
	b := &storeBlock{label: label, reason: reason, since: time.Now()}
	if prev, loaded := storeReadBlocked.LoadOrStore(path, b); loaded {
		pb := prev.(*storeBlock)
		pb.mu.Lock()
		same := pb.reason == reason
		if !same {
			pb.reason = reason
			pb.needRestart = false
			pb.probed = false
		}
		pb.mu.Unlock()
		if same {
			return
		}
	}
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
// qualify. Called on every successful load; at runtime the same lift happens
// from blockedIfUnreadable when a probe finds the file gone.
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
	block  *storeBlock
}

func (e *errStoreReadBlocked) Error() string {
	return "refusing to overwrite an unreadable session store file: " + e.reason
}

// warnDue reports whether the caller should log this failure at Warn (true) or
// demote it to Debug: the first failure per path warns, then one per
// storeBlockedWarnEvery.
func (e *errStoreReadBlocked) warnDue() bool {
	if e.block == nil {
		return true
	}
	e.block.mu.Lock()
	defer e.block.mu.Unlock()
	now := time.Now()
	if !e.block.lastWarn.IsZero() && now.Sub(e.block.lastWarn) < storeBlockedWarnEvery {
		return false
	}
	e.block.lastWarn = now
	return true
}

// logStoreSaveFailure logs a save error at the right level: a blocked store
// throttles itself (see errStoreReadBlocked.warnDue), everything else warns.
func logStoreSaveFailure(msg string, err error) {
	var blocked *errStoreReadBlocked
	if errors.As(err, &blocked) && !blocked.warnDue() {
		slog.Debug(msg, "err", err)
		return
	}
	slog.Warn(msg, "err", err)
}

// blockedIfUnreadable returns a non-nil error when path must not be written.
// Every writer in store.go calls it first; that is the whole enforcement point.
// A blocked path is re-probed on the way (see the package comment), so an
// operator who moves the file aside gets persistence back on the next tick.
func blockedIfUnreadable(path string) error {
	if path == "" {
		return nil
	}
	v, ok := storeReadBlocked.Load(path)
	if !ok {
		return nil
	}
	b := v.(*storeBlock)
	if reprobeStoreBlock(path, b) {
		clearStoreReadUnreadable(path)
		return nil
	}
	b.mu.Lock()
	reason := b.reason
	b.mu.Unlock()
	return &errStoreReadBlocked{path: path, reason: reason, block: b}
}

// reprobeStoreBlock looks at the blocked file again and reports whether the
// block can lift. Only "nothing left on disk to clobber" lifts it.
func reprobeStoreBlock(path string, b *storeBlock) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		// Gone: the operator moved it aside. Any other Lstat failure leaves
		// the verdict as it was.
		return errors.Is(err, fs.ErrNotExist)
	}
	if fi.Mode().IsRegular() && fi.Size() == 0 {
		// Truncated: nothing in it to preserve, same as jsonfile.Load's own
		// reading of an empty file.
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.probed && fi.Size() == b.probeSize && fi.ModTime().Equal(b.probeMod) {
		return false
	}
	b.probed = true
	b.probeSize = fi.Size()
	b.probeMod = fi.ModTime()
	// LeaveCorrupt: a probe from a save tick must not rename anything.
	_, out, loadErr := jsonfile.Load[json.RawMessage](path, jsonfile.Options{
		MaxBytes: maxStoreFileBytes,
		Label:    b.label,
		Corrupt:  jsonfile.LeaveCorrupt,
	})
	if loadErr != nil || out != jsonfile.Parsed {
		return false
	}
	if !b.needRestart {
		b.needRestart = true
		b.reason = fmt.Sprintf("%s is readable again but its contents were not loaded (naozhi started without them); restart naozhi to load the file, or move it aside", b.label)
		slog.Warn("session store: file is readable again but was never loaded; saves stay blocked",
			"path", path, "label", b.label,
			"hint", "restart naozhi to load it, or move the file aside to resume saves with the current in-memory state")
	}
	return false
}

// StoreBlocks snapshots every blocked store file, sorted by path. Empty when
// every store file is writable.
func StoreBlocks() []StoreBlock {
	var out []StoreBlock
	storeReadBlocked.Range(func(k, v any) bool {
		path, _ := k.(string)
		b, _ := v.(*storeBlock)
		if b == nil {
			return true
		}
		b.mu.Lock()
		out = append(out, StoreBlock{
			Path:        path,
			Label:       b.label,
			Reason:      b.reason,
			Since:       b.since,
			NeedRestart: b.needRestart,
		})
		b.mu.Unlock()
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// StoreWriteBlocks is the Router-scoped view for /health: only the blocks on
// this Router's own store files, so a test harness with several Routers in one
// process does not see its neighbours'.
func (r *Router) StoreWriteBlocks() []StoreBlock {
	if r == nil || r.storePath == "" {
		return nil
	}
	mine := map[string]bool{
		r.storePath:                         true,
		knownIDsPath(r.storePath):           true,
		workspaceOverridesPath(r.storePath): true,
	}
	var out []StoreBlock
	for _, b := range StoreBlocks() {
		if mine[b.Path] {
			out = append(out, b)
		}
	}
	return out
}
