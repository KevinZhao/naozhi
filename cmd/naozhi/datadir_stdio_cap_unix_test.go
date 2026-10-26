//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/datadir"
)

// TestStdioCapsUse64MB drives addStdioCaps with regular O_APPEND files
// standing in for the init system's: the 64MB cap leaves a file just under it
// alone and truncates one at it.
func TestStdioCapsUse64MB(t *testing.T) {
	newLog := func(size int64) *os.File {
		f, err := os.OpenFile(filepath.Join(t.TempDir(), "out.log"), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		if err := f.Truncate(size); err != nil { // sparse; the cap only reads the tail
			t.Fatal(err)
		}
		if _, err := f.WriteString("last line\n"); err != nil {
			t.Fatal(err)
		}
		return f
	}
	size := func(f *os.File) int64 {
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}

	if stdioMaxSize != 64<<20 {
		t.Fatalf("stdioMaxSize = %d, want 64MB", stdioMaxSize)
	}
	under, over := newLog(stdioMaxSize-100), newLog(stdioMaxSize)
	s := datadir.NewSweeper(0)
	addStdioCaps(s, under, over)
	s.RunOnce()
	if got := size(under); got != stdioMaxSize-100+10 {
		t.Errorf("a file under the 64MB cap was changed: size %d", got)
	}
	if got := size(over); got >= stdioMaxSize {
		t.Errorf("a file over the 64MB cap was not truncated: size %d", got)
	}
}
