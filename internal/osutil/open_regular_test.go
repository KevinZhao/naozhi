package osutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenRegular_RegularFileAndSizeCap: the returned fd reads the file, Size
// comes from that fd, a cap is enforced only when positive, and a directory is
// refused rather than handed back as something to read.
func TestOpenRegular_RegularFileAndSizeCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, fi, err := OpenRegular(path, 0)
	if err != nil {
		t.Fatalf("OpenRegular(maxBytes=0): %v; 0 must mean no cap", err)
	}
	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, 6); err != nil || string(buf) != "6789" {
		t.Errorf("ReadAt = %q, %v; want 6789 through the returned fd", buf, err)
	}
	f.Close()
	if fi.Size() != 10 {
		t.Errorf("Size = %d, want 10", fi.Size())
	}

	if f, _, err := OpenRegular(path, 10); err != nil {
		t.Errorf("OpenRegular(maxBytes=size): %v; the cap is inclusive", err)
	} else {
		f.Close()
	}
	if _, _, err := OpenRegular(path, 9); !errors.Is(err, ErrTooLarge) {
		t.Errorf("OpenRegular(maxBytes=size-1) err = %v, want ErrTooLarge", err)
	}
	if _, _, err := OpenRegular(dir, 0); !errors.Is(err, ErrNotRegular) {
		t.Errorf("OpenRegular(dir) err = %v, want ErrNotRegular", err)
	}
	if _, _, err := OpenRegular(filepath.Join(dir, "absent"), 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenRegular(absent) err = %v, want ErrNotExist", err)
	}
}

// openTestRoot opens dir as an os.Root closed with the test.
func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// TestOpenRegularIn_RegularFileAndSizeCap: OpenRegularIn reads a regular
// file below the root through the fd it returns, enforces the cap, refuses
// a directory, and leaves no way out of the root by name.
func TestOpenRegularIn_RegularFileAndSizeCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "s", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("s", "workflows", "wf_a.json")
	if err := os.WriteFile(filepath.Join(dir, rel), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := openTestRoot(t, dir)
	f, fi, err := OpenRegularIn(root, rel, 10)
	if err != nil {
		t.Fatalf("OpenRegularIn: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, 6); err != nil || string(buf) != "6789" || fi.Size() != 10 {
		t.Errorf("ReadAt = %q, %v, size %d; want 6789 through the returned fd", buf, err, fi.Size())
	}
	f.Close()
	if _, _, err := OpenRegularIn(root, rel, 9); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over the cap: err = %v, want ErrTooLarge", err)
	}
	if _, _, err := OpenRegularIn(root, "s", 0); !errors.Is(err, ErrNotRegular) {
		t.Errorf("a directory: err = %v, want ErrNotRegular", err)
	}
	if _, _, err := OpenRegularIn(root, filepath.Join("s", "absent"), 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("absent: err = %v, want ErrNotExist", err)
	}
	outside := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{outside, filepath.Join("..", filepath.Base(filepath.Dir(outside)), "x")} {
		if f, _, err := OpenRegularIn(root, name, 0); err == nil {
			f.Close()
			t.Errorf("OpenRegularIn(%q) opened a file outside the root", name)
		}
	}
	d, err := OpenDirIn(root, filepath.Join("s", "workflows"))
	if err != nil {
		t.Fatalf("OpenDirIn: %v", err)
	}
	if ents, err := d.ReadDir(2); err != nil || len(ents) != 1 {
		t.Errorf("ReadDir = %v, %v; want the one file", ents, err)
	}
	d.Close()
	if _, err := OpenDirIn(root, rel); !errors.Is(err, ErrNotDir) {
		t.Errorf("OpenDirIn(file) err = %v, want ErrNotDir", err)
	}
}
