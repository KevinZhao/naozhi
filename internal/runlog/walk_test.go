package runlog

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func seedTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "runs")
	for _, spec := range []struct{ owner, name, body string }{
		{"owner1", "rec1.json", `{"n":1}`},
		{"owner1", "rec2.json", `{"n":2}`},
		{"owner2", "rec3.json", `{"n":3}`},
		// Not records: the corrupt sibling jsonfile.Load leaves behind, a
		// temp file from an interrupted atomic write, and a stray note.
		{"owner1", "rec9.json.corrupt.1700000000", `{}`},
		{"owner1", "rec9.json.tmp-123", `{}`},
		{"owner1", "README", `hi`},
	} {
		dir := filepath.Join(root, spec.owner)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, spec.name), []byte(spec.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A nested directory inside an owner dir must not be walked into — and it is
	// named with a .json suffix on purpose, because an extension check alone
	// would happily yield a directory as a record.
	if err := os.MkdirAll(filepath.Join(root, "owner1", "nested.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "owner1", "nested.json", "deep.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A loose file at the root level is not an owner directory.
	if err := os.WriteFile(filepath.Join(root, "loose.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestWalkRecords_YieldsOnlyRecordFiles pins what counts as a run record: the
// .json files exactly one level under an owner directory, and nothing else —
// not the .corrupt evidence sibling, not an interrupted write's .tmp-*, not a
// deeper nesting, not a loose file at the root.
func TestWalkRecords_YieldsOnlyRecordFiles(t *testing.T) {
	t.Parallel()
	root := seedTree(t)

	var got []string
	var bodies []string
	WalkRecords(root, func(path string, err error) {
		t.Errorf("unexpected walk error at %s: %v", path, err)
	}, func(rec Record) {
		got = append(got, rec.OwnerDir+"/"+rec.ID)
		bodies = append(bodies, string(rec.Raw))
	})
	sort.Strings(got)
	sort.Strings(bodies)

	want := []string{"owner1/rec1", "owner1/rec2", "owner2/rec3"}
	if len(got) != len(want) {
		t.Fatalf("walked %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("walked[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if bodies[0] != `{"n":1}` {
		t.Errorf("Raw not carried through: %q", bodies[0])
	}
}

// TestWalkRecords_MissingRootIsSilent: a store that never persisted anything
// has nothing to walk, and `cost backfill` on a fresh install must not report
// that as an error.
func TestWalkRecords_MissingRootIsSilent(t *testing.T) {
	t.Parallel()
	calls, errs := 0, 0
	WalkRecords(filepath.Join(t.TempDir(), "never-created"),
		func(string, error) { errs++ },
		func(Record) { calls++ })
	if calls != 0 || errs != 0 {
		t.Errorf("calls=%d errs=%d, want 0/0 for a missing root", calls, errs)
	}
	WalkRecords("", nil, func(Record) { t.Error("empty root must walk nothing") })
}

// TestWalkRecords_UnreadableEntryDoesNotAbortTheWalk: a backfill that gives up
// on the first unreadable file imports nothing, so the error is reported and
// the walk continues.
func TestWalkRecords_UnreadableEntryDoesNotAbortTheWalk(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runs")
	for _, owner := range []string{"owner1", "owner2"} {
		if err := os.MkdirAll(filepath.Join(root, owner), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, owner, "rec.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// owner1's record is unreadable; owner3's directory is unreadable.
	if err := os.Chmod(filepath.Join(root, "owner1", "rec.json"), 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(filepath.Join(root, "owner1", "rec.json"), 0o600) }()
	blocked := filepath.Join(root, "owner3")
	if err := os.MkdirAll(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(blocked, 0o700) }()

	seen, errPaths := 0, 0
	WalkRecords(root, func(string, error) { errPaths++ }, func(Record) { seen++ })

	if seen == 0 {
		t.Fatal("the walk must keep going past unreadable entries")
	}
	if errPaths == 0 {
		t.Skip("running as root: nothing was unreadable, error path not exercised")
	}
	// owner2's record is readable regardless of the two failures above.
	if seen != 1 {
		t.Errorf("walked %d readable records, want 1 (owner2's)", seen)
	}
}

// TestWalkRecords_NilOnErrorTolerated: the callback is optional, and a caller
// that does not care must not need a no-op closure.
func TestWalkRecords_NilOnErrorTolerated(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runs")
	blocked := filepath.Join(root, "owner1")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	// Make the owner dir unreadable AFTER creating it (MkdirAll cannot create a
	// child of a 0000 parent), so the walk hits the error path with no callback.
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(blocked, 0o700) }()

	WalkRecords(root, nil, func(Record) {}) // must not panic
}
