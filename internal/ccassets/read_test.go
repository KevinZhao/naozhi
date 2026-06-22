package ccassets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/assets"
)

// TestReadCapped_PlainFile verifies a normal regular file reads back intact.
func TestReadCapped_PlainFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "asset.md")
	want := []byte("hello skill\n")
	if err := os.WriteFile(p, want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readCapped(p, maxRawBytes)
	if err != nil {
		t.Fatalf("readCapped: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q; want %q", got, want)
	}
}

// TestReadCapped_RejectsSymlink pins R202606d-SEC-1: even when a symlink points
// at a perfectly legitimate file inside the same root, readCapped must refuse
// to open it because O_NOFOLLOW rejects a symlinked final component. This is
// the post-EvalSymlinks TOCTOU swap an attacker with ~/.claude write access
// could perform between resolveUnder and the open.
func TestReadCapped_RejectsSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "real.md")
	if err := os.WriteFile(target, []byte("legit"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	if _, err := readCapped(link, maxRawBytes); err == nil {
		t.Fatal("readCapped opened a symlink; O_NOFOLLOW should have refused it")
	}
}

// TestReadCapped_OverCap verifies the cap+1 over-size detection still returns
// ErrTooLarge rather than a truncated body.
func TestReadCapped_OverCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "big.md")
	if err := os.WriteFile(p, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCapped(p, 32); !errors.Is(err, assets.ErrTooLarge) {
		t.Fatalf("readCapped over cap = %v; want ErrTooLarge", err)
	}
}
