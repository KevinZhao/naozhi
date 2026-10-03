//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/datadir"
)

// TestStdioCapsUseTheConfiguredSize drives addStdioCaps with regular O_APPEND
// files standing in for the init system's: the default cap leaves a file just
// under 64MB alone, and a configured cap truncates.
func TestStdioCapsUseTheConfiguredSize(t *testing.T) {
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

	const defaultStdioMaxSize = 64 << 20
	if got := (&config.Config{}).LogStdioMaxSize(); got != defaultStdioMaxSize {
		t.Fatalf("LogStdioMaxSize default = %d, want 64MB", got)
	}
	under, over := newLog(defaultStdioMaxSize-100), newLog(defaultStdioMaxSize)
	s := datadir.NewSweeper(0)
	addStdioCaps(s, &config.Config{}, under, over)
	s.RunOnce()
	if got := size(under); got != defaultStdioMaxSize-100+10 {
		t.Errorf("a file under the 64MB default was changed: size %d", got)
	}
	if got := size(over); got >= defaultStdioMaxSize {
		t.Errorf("a file over the 64MB default was not truncated: size %d", got)
	}

	small := newLog(2 << 20)
	cfg := loadConfigBody(t, "log:\n  stdio_max_size: 1MB\n")
	s = datadir.NewSweeper(0)
	addStdioCaps(s, cfg, small, small)
	s.RunOnce()
	if got := size(small); got >= 1<<20 {
		t.Errorf("stdio_max_size \"1MB\" left a 2MB file at %d bytes", got)
	}
}
