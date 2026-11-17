//go:build unix

package attachment

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestGCWithRefs_FIFOMetaDoesNotWedge: a FIFO planted as a payload's .meta in a
// shared workspace must not block the sweep in open(2) — ctx cannot interrupt
// that, and a stuck daemon tick holds up shutdown. The unreadable sidecar keeps
// the payload, as any other unreadable meta does.
func TestGCWithRefs_FIFOMetaDoesNotWedge(t *testing.T) {
	ws := t.TempDir()
	now := time.Now().UTC()
	dayDir := filepath.Join(ws, Dir, now.AddDate(0, 0, -10).Format("2006-01-02"))
	if err := os.MkdirAll(dayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dayDir, "x.png")
	if err := os.WriteFile(payload, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	metaPath := MetaPathFor(payload)
	if err := syscall.Mkfifo(metaPath, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	done := make(chan GCResult, 1)
	go func() {
		res, _ := GCWithRefs(context.Background(), ws, GCOptions{
			UploadTTL: 7 * 24 * time.Hour,
			RefTTL:    DefaultRefTTL,
			Now:       now,
		})
		done <- res
	}()
	select {
	case res := <-done:
		if res.Removed != 0 {
			t.Errorf("Removed=%d, want 0: an unreadable meta keeps the payload", res.Removed)
		}
		if _, err := os.Stat(payload); err != nil {
			t.Errorf("payload gone: %v", err)
		}
	case <-time.After(2 * time.Second):
		// Opening the write end releases the blocked reader.
		if w, err := os.OpenFile(metaPath, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
		<-done
		t.Fatal("GC sweep blocked opening a FIFO .meta")
	}
}
