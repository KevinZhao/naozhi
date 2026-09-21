package runlog

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newLayout(t *testing.T, root string) *Layout {
	t.Helper()
	return New(Options{Root: root, Label: "test run"})
}

// TestNew_SymlinkedRootDisablesStore: a root that is a symlink redirects every
// record out of the data dir, and MkdirAll does not error on a
// symlink-to-directory — so the store refuses to run at all rather than
// writing through it (#825).
func TestNew_SymlinkedRootDisablesStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "runs")
	if err := os.Symlink(elsewhere, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	l := newLayout(t, root)

	if l.Enabled() {
		t.Fatal("symlinked root must disable the store")
	}
	if err := l.WriteRecord("owner1", "rec1", []byte(`{}`)); err != nil {
		t.Errorf("WriteRecord on a disabled layout must be a silent no-op, got %v", err)
	}
	ents, _ := os.ReadDir(elsewhere)
	if len(ents) != 0 {
		t.Errorf("a disabled store wrote %d entries through the symlink", len(ents))
	}
}

// TestNew_NonDirectoryRootDisablesStore is the same guard for a plain file
// sitting where the root belongs.
func TestNew_NonDirectoryRootDisablesStore(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runs")
	if err := os.WriteFile(root, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}

	if newLayout(t, root).Enabled() {
		t.Error("a regular file as root must disable the store")
	}
}

// TestNew_TightensLooseRootPerm: MkdirAll honours perm only for directories it
// creates, so a pre-existing 0755 root would keep letting other OS users on
// the box enumerate owner IDs and read records (#504).
func TestNew_TightensLooseRootPerm(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	if !newLayout(t, root).Enabled() {
		t.Fatal("a loose-perm root is usable, not fatal")
	}

	fi, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("root mode = %v, want 0700 — other OS users can still enumerate owners", perm)
	}
}

// TestNew_OwnerDirIsOwnerOnly pins the same property one level down: the
// directory the layout creates for an owner must not be group/world readable.
func TestNew_OwnerDirIsOwnerOnly(t *testing.T) {
	t.Parallel()
	l := newLayout(t, filepath.Join(t.TempDir(), "runs"))
	dir, err := l.EnsureOwnerDir("owner1")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("owner dir mode = %v, want 0700", perm)
	}
}

// TestEnsureOwnerDir_RefusesSymlinkedOwnerDir: the per-owner guard, same
// reasoning as the root guard one level down.
func TestEnsureOwnerDir_RefusesSymlinkedOwnerDir(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "runs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "owner1")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	l := newLayout(t, root)

	if _, err := l.EnsureOwnerDir("owner1"); err == nil {
		t.Error("a symlinked owner dir must be refused")
	}
	if err := l.WriteRecord("owner1", "rec1", []byte(`{}`)); err == nil {
		t.Error("WriteRecord must fail when the owner dir is a symlink")
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("wrote %d entries through the owner-dir symlink", len(ents))
	}
}

// TestEnsureOwnerDir_RevalidatesAfterSwap is the property the ensured-marker
// fast path must NOT break: the guard runs on every call, so a directory
// swapped for a symlink after the first successful append is still caught
// (#1968). A marker-gated guard passes the first assertion and fails this one.
func TestEnsureOwnerDir_RevalidatesAfterSwap(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "runs")
	l := newLayout(t, root)

	dir, err := l.EnsureOwnerDir("owner1")
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	// Swap the real directory for a symlink, exactly as an attacker racing the
	// second append would.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := l.EnsureOwnerDir("owner1"); err == nil {
		t.Error("the swapped-in symlink must be caught on the SECOND call too")
	}
}

// TestOwnerDir_RefusesEscape: containment is defence in depth behind the
// caller's ID validation — a caller that forgets to validate must still not be
// able to walk out of the root (#484).
//
// The table came from internal/cron's TestRunStore_Append_RootGuardLogic, which
// asserted the same cases against a copy of the predicate re-typed inside the
// test rather than against the production path. That made it a test of
// filepath.Rel arithmetic: the guard could be deleted from the store and the
// test would still pass. Here the same cases drive the real function.
func TestOwnerDir_RefusesEscape(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runs")
	l := newLayout(t, root)

	for _, tc := range []struct {
		name   string
		owner  string
		reject bool
	}{
		{"normal-hex-id", "0123456789abcdef", false},
		{"escape-via-dotdot", "..", true},
		{"escape-deeper", "../..", true},
		{"escape-into-sibling", "../other-runs", true},
		{"escape-mid-path", "a/../..", true},
		// An owner id carrying a separator is refused rather than silently
		// joined into a nested path: one owner is one directory component.
		{"absolute-path", string(filepath.Separator) + "etc", true},
		{"nested-path", "a" + string(filepath.Separator) + "b", true},
		{"empty-id", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := l.OwnerDir(tc.owner)
			if tc.reject {
				if err == nil {
					t.Fatalf("OwnerDir(%q) = %q, want refused", tc.owner, dir)
				}
				return
			}
			if err != nil {
				t.Fatalf("OwnerDir(%q) = %v, want accepted", tc.owner, err)
			}
			if filepath.Dir(dir) != root {
				t.Errorf("OwnerDir(%q) = %q, want a direct child of %q", tc.owner, dir, root)
			}
		})
	}
}

// TestRecordPath_RefusesSeparators keeps a record ID from becoming a path.
func TestRecordPath_RefusesSeparators(t *testing.T) {
	t.Parallel()
	l := newLayout(t, filepath.Join(t.TempDir(), "runs"))

	for _, id := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := l.RecordPath("owner1", id); err == nil {
			t.Errorf("RecordPath(%q) must be refused", id)
		}
	}
	path, err := l.RecordPath("owner1", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "run-1.json" {
		t.Errorf("record path = %q, want the id with a .json suffix", path)
	}
}

// TestWriteRecord_RoundTripsAndCountsFailures: the happy path lands the bytes,
// and a failure bumps the non-disk-full counter — the only operator-visible
// signal a best-effort store has (#1338).
func TestWriteRecord_RoundTripsAndCountsFailures(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runs")
	l := newLayout(t, root)

	if err := l.WriteRecord("owner1", "rec1", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "owner1", "rec1.json"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("record = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(root, "owner1", "rec1.json")); err == nil {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("record mode = %v, want 0600", perm)
		}
	}
	if full, other := l.WriteFailedTotals(); full != 0 || other != 0 {
		t.Errorf("counters = (%d,%d) after a successful write, want (0,0)", full, other)
	}

	// Make the owner directory unwritable so the atomic write cannot land.
	dir := filepath.Join(root, "owner2")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := l.WriteRecord("owner2", "rec2", []byte(`{}`)); err == nil {
		t.Skip("write into a 0500 dir succeeded (running as root?); counter path not exercised")
	}
	if full, other := l.WriteFailedTotals(); other != 1 || full != 0 {
		t.Errorf("counters = (%d,%d), want (0,1) — a non-ENOSPC failure must be counted separately", full, other)
	}
}

// TestOwnerDirName_MapsThroughHook: runhistory cannot use an owner ID as a
// directory name (a session key carries ':' and user content), so the mapping
// is a hook rather than a hard-coded identity.
func TestOwnerDirName_MapsThroughHook(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runs")
	l := New(Options{
		Root:         root,
		Label:        "test run",
		OwnerDirName: func(owner string) string { return "h-" + strings.ReplaceAll(owner, ":", "_") },
	})

	if err := l.WriteRecord("dashboard:direct:x", "rec1", []byte(`{}`)); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "h-dashboard_direct_x", "rec1.json")); err != nil {
		t.Errorf("record did not land under the mapped dir name: %v", err)
	}
}

// TestLock_IsPerOwnerAndStable: two owners must not serialise against each
// other, and the same owner must get the same mutex every time (a fresh mutex
// per call would serialise nothing).
func TestLock_IsPerOwnerAndStable(t *testing.T) {
	t.Parallel()
	l := newLayout(t, filepath.Join(t.TempDir(), "runs"))

	a1, a2 := l.Lock("owner1"), l.Lock("owner1")
	if a1 != a2 {
		t.Error("Lock must return the same mutex for the same owner")
	}
	if a1 == l.Lock("owner2") {
		t.Error("Lock must not share one mutex across owners")
	}

	a1.Lock()
	// A different owner's lock stays free while owner1 is held.
	b := l.Lock("owner2")
	if !b.TryLock() {
		t.Error("holding owner1's lock must not block owner2")
	}
	b.Unlock()
	a1.Unlock()
}

// TestForgetOwner_DropsLockAndMarker: the lock set must track the live owner
// set rather than growing for the process lifetime (#971), and a re-created
// owner must go through MkdirAll again instead of trusting a stale marker.
func TestForgetOwner_DropsLockAndMarker(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runs")
	l := newLayout(t, root)

	before := l.Lock("owner1")
	dir, err := l.EnsureOwnerDir("owner1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	l.ForgetOwner("owner1")

	if after := l.Lock("owner1"); after == before {
		t.Error("ForgetOwner must drop the mutex so the live set tracks live owners")
	}
	l.ForgetOwner("owner1")
	if n := l.OwnerLockCount(); n != 0 {
		t.Errorf("OwnerLockCount = %d after forgetting the only owner, want 0", n)
	}
	if _, err := l.EnsureOwnerDir("owner1"); err != nil {
		t.Fatalf("re-ensure after ForgetOwner: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("owner dir was not re-created after ForgetOwner: %v", err)
	}
}

// TestAssertLockHeld_WarnsOnlyWhenFree: the assertion is the machine-checked
// half of every *Locked-suffix contract, so it must fire when the lock is free
// and stay silent when the caller really holds it (a always-warn version would
// be noise the next reader learns to ignore).
func TestAssertLockHeld_WarnsOnlyWhenFree(t *testing.T) {
	// Not parallel: swaps the default slog handler.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	l := newLayout(t, filepath.Join(t.TempDir(), "runs"))

	l.AssertLockHeld("owner1")
	if !strings.Contains(buf.String(), "owner lock not held") {
		t.Errorf("a free lock must warn; log = %q", buf.String())
	}

	buf.Reset()
	lock := l.Lock("owner1")
	lock.Lock()
	l.AssertLockHeld("owner1")
	lock.Unlock()
	if buf.Len() != 0 {
		t.Errorf("a held lock must stay silent; log = %q", buf.String())
	}
}

// TestDisabledLayout_IsTotalNoop: "" root is how both stores spell "no
// persistence", and every method has to survive it — callers deliberately do
// not nil-check.
func TestDisabledLayout_IsTotalNoop(t *testing.T) {
	t.Parallel()
	l := New(Options{Root: "", Label: "test run"})

	if l.Enabled() || l.Root() != "" {
		t.Error("an empty root must disable the layout")
	}
	if err := l.WriteRecord("owner1", "rec1", []byte(`{}`)); err != nil {
		t.Errorf("WriteRecord = %v, want nil no-op", err)
	}
	if _, err := l.OwnerDir("owner1"); err == nil {
		t.Error("OwnerDir must report the store is disabled rather than name a path")
	}
	l.AssertLockHeld("owner1") // must not panic
	l.ForgetOwner("owner1")    // must not panic
	if full, other := l.WriteFailedTotals(); full != 0 || other != 0 {
		t.Errorf("counters = (%d,%d)", full, other)
	}
	var nilLayout *Layout
	if nilLayout.Enabled() {
		t.Error("a nil *Layout must report disabled")
	}
	nilLayout.ForgetOwner("owner1")
	if full, other := nilLayout.WriteFailedTotals(); full != 0 || other != 0 {
		t.Error("a nil *Layout must report zero counters")
	}
}
