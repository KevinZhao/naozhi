//go:build unix

package ccassets

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/assets"
)

// mkfifo makes a FIFO at path; Cleanup opens it read-write so a reader an
// unfixed open left blocked is released instead of leaking past the test.
func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	t.Cleanup(func() {
		if f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
}

// within fails the test when fn has not returned after a few seconds: an
// open(2) of a FIFO with no writer never returns on its own.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s blocked on a FIFO", what)
	}
}

// A same-user agent can leave a FIFO anywhere under ~/.claude; the asset
// panel must still list the real assets and refuse the FIFO, not hang.
func TestScan_FIFOsDoNotBlock(t *testing.T) {
	home := t.TempDir()
	writeSkill(t, filepath.Join(home, "skills"), "real",
		"---\nname: real\ndescription: d\n---\nbody")
	for _, p := range []string{
		"agents/x.md",
		"commands/x.md",
		"skills/fifo/SKILL.md",
		".mcp.json",
		"plugins/installed_plugins.json",
		"plugins/known_marketplaces.json",
	} {
		mkfifo(t, filepath.Join(home, p))
	}

	var inv *assets.Inventory
	var err error
	within(t, "Scan", func() {
		inv, err = NewClaudeProvider().Scan(assets.ScanRequest{Home: home})
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(inv.Assets) != 1 || inv.Assets[0].Name != "real" {
		t.Errorf("Assets = %+v, want only the real skill", inv.Assets)
	}
}

func TestReadRaw_FIFORefusedWithoutBlocking(t *testing.T) {
	home := t.TempDir()
	mkfifo(t, filepath.Join(home, "agents", "x.md"))

	var err error
	within(t, "ReadRaw", func() {
		_, err = NewClaudeProvider().ReadRaw(assets.RawRequest{
			Home: home,
			Ref:  assets.Ref{Source: assets.Source{Kind: "user"}, RelPath: "agents/x.md"},
		})
	})
	if err == nil {
		t.Fatal("ReadRaw read a FIFO; want it refused")
	}
}

// The scan still follows a final-component symlink inside ~/.claude (an agent
// linked in from a dotfiles checkout), unlike ReadRaw's O_NOFOLLOW open.
func TestScan_FollowsSymlinkedAgentFile(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "linked.md")
	if err := os.WriteFile(target, []byte("---\nname: linked\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, "agents", "linked.md")); err != nil {
		t.Fatal(err)
	}
	inv, err := NewClaudeProvider().Scan(assets.ScanRequest{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Assets) != 1 || inv.Assets[0].Name != "linked" {
		t.Errorf("Assets = %+v, want the symlinked agent", inv.Assets)
	}
}
