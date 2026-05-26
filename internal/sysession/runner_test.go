package sysession

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewRunner_BinPathValidation pins R247-SEC-19: an absolute BinPath that
// is not a regular executable file MUST be rejected at construction time
// rather than degrading at first Tick. A relative BinPath that resolveBin
// PathFromEnv could not lift to absolute is allowed through unchanged so
// the documented "degrade gracefully" path stays intact.
func TestNewRunner_BinPathValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}

	regularNonExec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(regularNonExec, []byte("not exec"), 0o644); err != nil {
		t.Fatalf("write noexec: %v", err)
	}
	regularExec := filepath.Join(dir, "claude-fake")
	if err := os.WriteFile(regularExec, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	dirAsBin := filepath.Join(dir, "binAsDir")
	if err := os.MkdirAll(dirAsBin, 0o755); err != nil {
		t.Fatalf("mkdir binAsDir: %v", err)
	}

	t.Run("regular_executable_passes", func(t *testing.T) {
		t.Parallel()
		_, err := NewRunner(RunnerConfig{BinPath: regularExec, WorkDir: work})
		if err != nil {
			t.Fatalf("NewRunner with valid abs binary should succeed, got %v", err)
		}
	})

	t.Run("absolute_missing_rejected", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(dir, "does-not-exist")
		_, err := NewRunner(RunnerConfig{BinPath: missing, WorkDir: work})
		if err == nil {
			t.Fatal("NewRunner with missing abs path should error")
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("want errors.Is os.ErrNotExist, got %v", err)
		}
	})

	t.Run("absolute_dir_rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewRunner(RunnerConfig{BinPath: dirAsBin, WorkDir: work})
		if err == nil {
			t.Fatal("NewRunner with directory BinPath should error")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("want regular-file error, got %v", err)
		}
	})

	t.Run("absolute_nonexec_rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewRunner(RunnerConfig{BinPath: regularNonExec, WorkDir: work})
		if err == nil {
			t.Fatal("NewRunner with non-executable BinPath should error")
		}
		if !strings.Contains(err.Error(), "not executable") {
			t.Errorf("want executable error, got %v", err)
		}
	})

	t.Run("unresolved_relative_passes", func(t *testing.T) {
		t.Parallel()
		// Stub osStat so resolveBinPathFromEnv finds nothing in PATH and
		// cfg.BinPath stays as a relative literal — the IsAbs guard then
		// skips the validation block, matching the documented "degrade
		// gracefully" path.
		prev := osStat
		osStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
		t.Cleanup(func() { osStat = prev })

		_, err := NewRunner(RunnerConfig{BinPath: "claude-not-installed", WorkDir: work})
		if err != nil {
			t.Fatalf("relative BinPath should still construct (lazy resolve), got %v", err)
		}
	})
}

// failingWriter returns errors on every Write call. Models a strings.Builder
// that has been corrupted, a wrapped fd that hit ENOSPC mid-stream, or any
// other inner sink that has fully failed.
type failingWriter struct {
	calls int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.calls++
	return 0, errors.New("inner-writer dead")
}

// TestRunner_Run_AppendsStderrToError pins R238-GO-12 (#804): when the
// underlying binary exits non-zero AND emits stderr, the returned error
// MUST embed a sanitized head of that stderr so the dashboard breaker's
// last_error field carries a meaningful diagnostic. Previously stderr
// only reached the slog Warn output and operators saw "exit status 1"
// in the breaker UI.
func TestRunner_Run_AppendsStderrToError(t *testing.T) {
	t.Parallel()
	// Build a shell-script BinPath that emits a known stderr marker and
	// exits non-zero. Avoids depending on a real claude binary.
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	bin := filepath.Join(dir, "fake-claude")
	script := "#!/bin/sh\necho 'NAOZHI_TEST_STDERR_MARKER_42' 1>&2\nexit 7\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}

	r, err := NewRunner(RunnerConfig{BinPath: bin, WorkDir: work})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, err = r.Run(context.Background(), "ignored prompt")
	if err == nil {
		t.Fatal("Run on exit-7 binary should error")
	}
	if !strings.Contains(err.Error(), "NAOZHI_TEST_STDERR_MARKER_42") {
		t.Errorf("error must embed sanitized stderr head; got: %v", err)
	}
	if !strings.Contains(err.Error(), "stderr:") {
		t.Errorf("error must label the stderr section; got: %v", err)
	}
}

// TestLimitedWriter_StopsCallingFailedInnerWriter pins R238-GO-5 (#794):
// once the inner io.Writer has reported a non-nil error, limitedWriter
// MUST NOT call it again. The previous shape kept invoking inner.Write on
// every subsequent chunk because lw.n never grew (failed writes return
// written=0), so the cap-overflow fast-path never engaged either. exec.Cmd
// would burn a syscall per stderr line for the rest of the subprocess
// lifetime. Verify the failed flag short-circuits to the same
// swallow-and-claim-success behaviour the cap-overflow path uses.
func TestLimitedWriter_StopsCallingFailedInnerWriter(t *testing.T) {
	t.Parallel()
	fw := &failingWriter{}
	lw := &limitedWriter{w: fw, max: 1024}

	// First Write triggers the inner-writer error. We still must report
	// (len(p), nil) so exec.Cmd's pump treats the chunk as accepted.
	chunk := []byte("first stderr line\n")
	n, err := lw.Write(chunk)
	if n != len(chunk) || err != nil {
		t.Fatalf("first Write = (%d, %v), want (%d, nil)", n, err, len(chunk))
	}
	if fw.calls != 1 {
		t.Fatalf("first Write should have called inner once, got %d", fw.calls)
	}
	if !lw.failed {
		t.Errorf("limitedWriter.failed should be set after inner error")
	}

	// Subsequent Writes must NOT touch the inner writer at all — the
	// failed flag should short-circuit. Without the fix, every line
	// would re-enter inner.Write.
	for i := 0; i < 5; i++ {
		n, err := lw.Write([]byte("subsequent line\n"))
		if n != 16 || err != nil {
			t.Fatalf("post-fail Write %d returned (%d, %v), want (16, nil)", i, n, err)
		}
	}
	if fw.calls != 1 {
		t.Errorf("inner writer was called %d times after first failure; want exactly 1", fw.calls)
	}
}
