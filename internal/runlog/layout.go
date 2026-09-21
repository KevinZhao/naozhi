// Package runlog owns the on-disk layout shared by naozhi's per-owner run
// stores: a validated root, one directory per owner, one JSON record per run.
//
//	<root>/<ownerDir>/<recordID>.json
//
// J5 of #2548 (#2709). Two stores arrived at this same shape independently and
// then hardened it to very different levels:
//
//	internal/cron            root symlink refusal, 0700 correction, per-owner
//	                         dir symlink guard, root fsync on a fresh subdir,
//	                         containment re-check, split write-failure counters,
//	                         a lock-held assertion under `go test`
//	internal/session/runhistory  MkdirAll and a bare WriteFileAtomic
//
// None of those differences are decisions — the session store simply never had
// the incidents that taught cron each guard. Every one of them protects a
// property that is identical for both (a symlinked root redirects every write
// out of the data dir; a 0755 root leaks owner existence to other OS users on
// the box; an unsynced root can orphan a record across a crash), so the fix is
// one implementation rather than two water levels.
//
// What Layout deliberately does NOT own: the record type, its summary
// projection, the in-memory ring, and the retention algorithm. Those are where
// the two stores genuinely differ (cron trims on append with a cache-aware
// skip and decodes in parallel; runhistory trims at warm), and folding them in
// here would produce a third implementation instead of removing one.
package runlog

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/osutil"
)

// recordPerm / dirPerm are the modes every run store uses: records are
// operator-readable only, and the owner directory must not let another OS user
// enumerate owner IDs.
const (
	recordPerm fs.FileMode = 0o600
	dirPerm    fs.FileMode = 0o700
)

// Options configures one Layout. Root and Label are required; an empty Root
// disables the layout, which is how both stores spell "no persistence".
type Options struct {
	// Root is the tree the layout owns, already named by the caller via
	// datadir.Layout (RunsRoot / SessionRunsRoot). Empty disables.
	Root string
	// Label names the store in logs, e.g. "cron run". Required so a log line
	// says which store hit the guard.
	Label string
	// OwnerDirName maps an owner ID to its directory name. nil means identity,
	// which is only safe when the caller validates the ID (cron's hex IDs);
	// runhistory passes a hash because a session key carries ':' and
	// user-controlled content.
	OwnerDirName func(owner string) string
}

// Layout is a validated run-record tree plus the per-owner locks and the
// write-failure counters that go with it. Safe for concurrent use. A nil or
// disabled *Layout is a no-op for every method, so callers never nil-check.
type Layout struct {
	root     string
	label    string
	dirName  func(string) string
	disabled bool

	// locks maps owner -> *sync.Mutex, lazily allocated and reclaimed by
	// ForgetOwner so the live set tracks the live owner set (#971).
	locks sync.Map
	// ensured records owners EnsureOwnerDir has already created, to skip the
	// idempotent MkdirAll + root fsync on a long-lived owner's every append.
	// A cache of syscalls, never of correctness: the symlink guard below runs
	// on every call, and a failed MkdirAll leaves no marker so the next call
	// retries (#1968).
	ensured sync.Map

	writeFailedDiskFullTotal atomic.Int64
	writeFailedOtherTotal    atomic.Int64
}

// New validates root and returns the Layout. A root that is a symlink or not a
// directory disables the store rather than writing through it: a pre-created
// `<dataDir>/runs -> /etc` would land every record outside the data dir, and
// MkdirAll does not error on a symlink-to-directory (#825).
func New(opts Options) *Layout {
	if opts.Root == "" {
		return &Layout{disabled: true}
	}
	label := opts.Label
	if label == "" {
		label = "runlog"
	}
	// Abs also cleans `..` / `.` segments; if it fails (CWD gone) Clean still
	// strips traversal.
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		slog.Warn(label+": root Abs failed; falling back to Clean", "path", opts.Root, "err", err)
		root = filepath.Clean(opts.Root)
	}
	if err := os.MkdirAll(root, dirPerm); err != nil {
		// Non-fatal: EnsureOwnerDir's MkdirAll still creates the tree, and the
		// Lstat below is what decides whether the store is usable at all.
		slog.Warn(label+": mkdir root failed", "root", root, "err", err)
	}
	// MkdirAll honours perm only on directories it creates, so a pre-existing
	// 0755 root keeps its mode and leaks owner existence + content to other OS
	// users. Chmod the leaf; log and continue because a bind-mounted container
	// root may not be chmod-able (#504).
	if fi, err := os.Lstat(root); err == nil && fi.Mode()&fs.ModeSymlink == 0 && fi.IsDir() {
		if perm := fi.Mode().Perm(); perm != dirPerm {
			if cerr := os.Chmod(root, dirPerm); cerr != nil {
				slog.Warn(label+": chmod root to 0700 failed",
					"root", root, "had_mode", perm.String(), "err", cerr)
			} else {
				slog.Info(label+": corrected root mode to 0700",
					"root", root, "had_mode", perm.String())
			}
		}
	}
	if fi, err := os.Lstat(root); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			slog.Error(label+": root is a symlink or non-directory; disabling store",
				"root", root, "mode", fi.Mode().String())
			return &Layout{disabled: true}
		}
	}
	return &Layout{root: root, label: label, dirName: opts.OwnerDirName}
}

// Enabled reports whether the layout persists anything. Callers spell their
// own "store off" predicate in terms of this rather than comparing to nil.
func (l *Layout) Enabled() bool { return l != nil && !l.disabled }

// Root returns the validated root, or "" when disabled.
func (l *Layout) Root() string {
	if !l.Enabled() {
		return ""
	}
	return l.root
}

// dirFor maps an owner to its directory name via OwnerDirName (identity when
// unset).
func (l *Layout) dirFor(owner string) string {
	if l.dirName == nil {
		return owner
	}
	return l.dirName(owner)
}

// OwnerDir returns the owner's directory path without touching the disk. The
// containment check is defence in depth behind the caller's ID validation: a
// future caller that forgets to validate must still not be able to escape the
// root through the join (#484).
func (l *Layout) OwnerDir(owner string) (string, error) {
	if !l.Enabled() {
		return "", fmt.Errorf("%s: store disabled", l.label)
	}
	if owner == "" {
		return "", fmt.Errorf("%s: empty owner id", l.label)
	}
	// One owner is one directory COMPONENT. A mapped name carrying a separator
	// would be joined into a nested path — contained, but silently not the
	// directory the caller named, and the same sloppiness RecordPath refuses for
	// record IDs. cron validates its hex IDs upstream and runhistory hashes,
	// so this only ever fires on a caller that forgot.
	name := l.dirFor(owner)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("%s: owner dir name must be a single path component", l.label)
	}
	dir := filepath.Join(l.root, name)
	rel, err := filepath.Rel(l.root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: owner dir escapes root", l.label)
	}
	return dir, nil
}

// EnsureOwnerDir returns the owner's directory, creating it on first use.
//
// The symlink guard runs on EVERY call, not just on a cache miss: a swap to a
// symlink after the first append would otherwise be waved through by the
// ensured-marker fast path (#1968). A fresh subdirectory also gets one root
// fsync, because WriteFileAtomic only fsyncs the record's immediate parent and
// a crash could otherwise orphan the directory entry (#976).
func (l *Layout) EnsureOwnerDir(owner string) (string, error) {
	dir, err := l.OwnerDir(owner)
	if err != nil {
		return "", err
	}
	if fi, lerr := os.Lstat(dir); lerr == nil {
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			slog.Error(l.label+": owner dir is a symlink or non-directory; refusing write",
				"dir", dir, "mode", fi.Mode().String(), "owner", osutil.SanitizeForLog(owner, 128))
			// Drop the stale marker so a later restore of the directory is re-validated.
			l.ensured.Delete(owner)
			return "", fmt.Errorf("%s: owner dir %q is not a plain directory", l.label, dir)
		}
	}
	if _, ok := l.ensured.Load(owner); ok {
		return dir, nil
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		// No marker: let the next call retry.
		return "", err
	}
	if err := osutil.SyncDir(l.root); err != nil {
		// Non-fatal: only crash-durability of the directory entry is degraded.
		slog.Debug(l.label+": root fsync skipped", "root", l.root, "err", err)
	}
	l.ensured.Store(owner, struct{}{})
	return dir, nil
}

// RecordPath names one record inside the owner's directory.
func (l *Layout) RecordPath(owner, recordID string) (string, error) {
	dir, err := l.OwnerDir(owner)
	if err != nil {
		return "", err
	}
	if recordID == "" || strings.ContainsAny(recordID, `/\`) || recordID == "." || recordID == ".." {
		return "", fmt.Errorf("%s: invalid record id", l.label)
	}
	return filepath.Join(dir, recordID+".json"), nil
}

// WriteRecord persists one record atomically, creating the owner directory on
// first use. A failure is counted (split so an operator can tell ENOSPC from
// EACCES / I/O) and returned; both stores log it rather than failing the
// caller's work, so the counter plus that log is the only operator-visible
// signal (#1338).
func (l *Layout) WriteRecord(owner, recordID string, payload []byte) error {
	if !l.Enabled() {
		return nil
	}
	if _, err := l.EnsureOwnerDir(owner); err != nil {
		return err
	}
	path, err := l.RecordPath(owner, recordID)
	if err != nil {
		return err
	}
	if err := osutil.WriteFileAtomic(path, payload, recordPerm); err != nil {
		if osutil.IsDiskFull(err) {
			l.writeFailedDiskFullTotal.Add(1)
		} else {
			l.writeFailedOtherTotal.Add(1)
		}
		return err
	}
	return nil
}

// WriteFailedTotals returns the split write-failure counters for /health and
// doctor.
func (l *Layout) WriteFailedTotals() (diskFull, other int64) {
	if l == nil {
		return 0, 0
	}
	return l.writeFailedDiskFullTotal.Load(), l.writeFailedOtherTotal.Load()
}

// Lock returns the mutex unique to owner, serialising that owner's ring and
// disk-subtree mutations. Leaf lock: never take a second owner's lock while
// holding one.
//
// Unlike every other method here, this one requires a non-nil Layout and will
// panic without one. That is deliberate: handing a throwaway mutex back to a
// caller that asked for serialisation would silently serialise nothing, and a
// store is always constructed with a layout (a disabled one for "no
// persistence", which still owns real locks).
func (l *Layout) Lock(owner string) *sync.Mutex {
	if v, ok := l.locks.Load(owner); ok {
		return v.(*sync.Mutex)
	}
	m := &sync.Mutex{}
	actual, _ := l.locks.LoadOrStore(owner, m)
	return actual.(*sync.Mutex)
}

// AssertLockHeld warns when Lock(owner) is currently free — the signature of a
// caller that violated a *Locked-suffix contract. Best-effort by design: it
// warns rather than panics because run history must never take the process
// down, false negatives under contention are accepted, and the TryLock probe
// only runs under `go test` since it sits on the append hot path (#961).
func (l *Layout) AssertLockHeld(owner string) {
	if !testing.Testing() || !l.Enabled() {
		return
	}
	lock := l.Lock(owner)
	if lock.TryLock() {
		lock.Unlock()
		slog.Warn(l.label+": owner lock not held by caller; *Locked-suffix contract violated",
			"owner", osutil.SanitizeForLog(owner, 128))
	}
}

// OwnerLockCount reports how many owner locks are currently live. The bound
// that #971 is about ("the map must not grow across create/delete churn") is
// otherwise unobservable from outside the package, which is how it regressed
// unnoticed in the first place.
func (l *Layout) OwnerLockCount() int {
	if l == nil {
		return 0
	}
	n := 0
	l.locks.Range(func(_, _ any) bool { n++; return true })
	return n
}

// ForgetOwner drops the owner's lock and ensured-marker. Called when the owner
// itself is deleted, so the live lock set tracks the live owner set rather
// than growing for the process lifetime (#971).
func (l *Layout) ForgetOwner(owner string) {
	if l == nil {
		return
	}
	l.locks.Delete(owner)
	l.ensured.Delete(owner)
}
