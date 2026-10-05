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
