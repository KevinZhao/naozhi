package attachment

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAged(t *testing.T, path string, data []byte, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestCollectRefs: every path an event log names is found, one straddling a
// read-chunk boundary included, and only the files with the asked extension
// are read.
func TestCollectRefs(t *testing.T) {
	dir := t.TempDir()
	writeAged(t, filepath.Join(dir, "a.log"),
		[]byte(`{"image_paths":["planner/attachments/2026-11-01/aaa.png","planner/attachments/2026-11-02/bbb.jpg"]}`), 0)
	// Pad so the reference starts a few bytes before the first chunk ends.
	big := bytes.Repeat([]byte("x"), refScanChunk-10)
	big = append(big, []byte(`"planner/attachments/2026-11-03/ccc.webp"`)...)
	writeAged(t, filepath.Join(dir, "b.log"), big, 0)
	writeAged(t, filepath.Join(dir, "b.idx"), []byte(`planner/attachments/2026-11-04/ddd.png`), 0)

	refs, err := CollectRefs(dir, ".log", "planner/attachments/")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2026-11-01/aaa.png", "2026-11-02/bbb.jpg", "2026-11-03/ccc.webp"} {
		if _, ok := refs[want]; !ok {
			t.Errorf("refs lack %s: %v", want, refs)
		}
	}
	if _, ok := refs["2026-11-04/ddd.png"]; ok {
		t.Error("a non-.log file was read")
	}
	if _, err := CollectRefs(filepath.Join(dir, "missing"), ".log", "planner/attachments/"); err == nil {
		t.Error("a missing dir gave no error; the caller would prune against an empty set")
	}
}

// TestPruneUnreferenced: an old payload no log names goes with its .meta; a
// named one, a young one and a file that is not an image stay; the emptied
// old day directory goes, one still holding a file stays.
func TestPruneUnreferenced(t *testing.T) {
	root := t.TempDir()
	const old, young = 30 * 24 * time.Hour, time.Hour
	gone := filepath.Join(root, "2026-10-01", "gone.png")
	writeAged(t, gone, []byte("png"), old)
	writeAged(t, MetaPathFor(gone), []byte(`{}`), old)
	named := filepath.Join(root, "2026-10-02", "named.jpg")
	writeAged(t, named, []byte("jpg"), old)
	fresh := filepath.Join(root, "2026-10-02", "fresh.png")
	writeAged(t, fresh, []byte("png"), young)
	other := filepath.Join(root, "2026-10-02", "notes.txt")
	writeAged(t, other, []byte("txt"), old)
	stray := filepath.Join(root, "manual", "x.png")
	writeAged(t, stray, []byte("png"), old)

	refs := map[string]struct{}{"2026-10-02/named.jpg": {}}
	res, err := PruneUnreferenced(root, 7*24*time.Hour, refs, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 || res.Bytes != 3 {
		t.Errorf("result %+v, want 1 removed, 3 bytes", res)
	}
	for _, p := range []string{gone, MetaPathFor(gone), filepath.Dir(gone)} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s still there", p)
		}
	}
	for _, p := range []string{named, fresh, other, stray} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
	// No reference set (the logs could not be read) prunes nothing.
	if res, _ := PruneUnreferenced(root, 7*24*time.Hour, nil, time.Now()); res.Removed != 0 {
		t.Errorf("nil refs removed %d", res.Removed)
	}
}
