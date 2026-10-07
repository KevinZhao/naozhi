//go:build unix

package subagent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// within runs fn and fails the test when it has not returned in 5s: a
// reader that opens a FIFO parks in open(2) until a writer appears.
func within(t *testing.T, what, fifo string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("%s blocked >5s on a FIFO", what)
		// Unpark the open so the goroutine does not outlive the test.
		if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		<-done
	}
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
}

// TestTranscriptReader_FIFOSwappedMidTail is §11.2's drill-in row: the
// transcript replaced by a FIFO between polls reads as gone on the next
// zero-byte poll, without blocking, and Close returns.
func TestTranscriptReader_FIFOSwappedMidTail(t *testing.T) {
	t.Parallel()
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	path := tmpFile(t, line+"\n")
	for _, r := range []*TranscriptReader{
		NewTranscriptReader(path),
		NewTranscriptReaderFrom(path, nil, func() (*os.File, error) {
			f, _, err := osutil.OpenRegular(path, 0)
			return f, err
		}, ReaderOpts{StripHarnessFraming: true}),
	} {
		if ents, err := r.Tail(); err != nil || len(ents) != 1 {
			t.Fatalf("first Tail: %v, %v", ents, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		mkfifo(t, path)
		within(t, "Tail", path, func() {
			if _, err := r.Tail(); !os.IsNotExist(err) {
				t.Errorf("Tail after the swap: %v, want not-exist", err)
			}
		})
		within(t, "second Tail", path, func() { _, _ = r.Tail() })
		within(t, "Close", path, func() { _ = r.Close() })
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTranscriptReader_RefusesFIFOAndSymlink: the default reader opens
// neither a FIFO nor a symlink, and reports both as gone.
func TestTranscriptReader_RefusesFIFOAndSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "agent-fifo.jsonl")
	mkfifo(t, fifo)
	within(t, "Read", fifo, func() {
		r := NewTranscriptReader(fifo)
		defer r.Close() //nolint:errcheck
		if _, err := r.Read(0, 0); !os.IsNotExist(err) {
			t.Errorf("FIFO: %v, want not-exist", err)
		}
	})

	target := tmpFile(t, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"x"}]}}`+"\n")
	link := filepath.Join(dir, "agent-link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	r := NewTranscriptReader(link)
	defer r.Close() //nolint:errcheck
	if ents, err := r.Read(0, 0); !os.IsNotExist(err) || len(ents) != 0 {
		t.Errorf("symlink: %v, %v; want not-exist", ents, err)
	}
}

// TestTranscriptReader_SymlinkSwapReadsAsGone: a rotation probe that finds
// a symlink where the file was drops the fd instead of following it.
func TestTranscriptReader_SymlinkSwapReadsAsGone(t *testing.T) {
	t.Parallel()
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"sessionId":"s","timestamp":"2026-05-10T10:00:00Z"}`
	path := tmpFile(t, line+"\n")
	target := tmpFile(t, line+"\n")
	r := NewTranscriptReaderFrom(path, nil, func() (*os.File, error) { return os.Open(path) }, ReaderOpts{})
	defer r.Close() //nolint:errcheck
	if _, err := r.Tail(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	if ents, err := r.Tail(); !os.IsNotExist(err) || len(ents) != 0 {
		t.Errorf("Tail after the swap: %v, %v; want not-exist", ents, err)
	}
}
