package sysession

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestVisionRunner_RunVision_CtxCancelReturnsCtxErr pins R20260606-GO-1 for
// the vision path: when the context is cancelled, RunVision must return
// ctx.Err() (context.Canceled), not the *exec.ExitError produced by killing
// the subprocess.  The old code combined ctx.Err()!=nil with
// errors.Is(err, context.Canceled) which is always false for the kill path.
func TestVisionRunner_RunVision_CtxCancelReturnsCtxErr(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	bin := filepath.Join(dir, "fake-claude-vision-sleep")
	script := "#!/bin/sh\nsleep 60\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}

	vr, err := NewVisionRunner(RunnerConfig{BinPath: bin, WorkDir: work})
	if err != nil {
		t.Fatalf("NewVisionRunner: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: cmd.Run will return ExitError from the kill

	_, runErr := vr.RunVision(ctx, []byte(`{}`+"\n"), "")
	if runErr == nil {
		t.Fatal("RunVision with cancelled ctx should return an error")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Errorf("RunVision with cancelled ctx: got %v, want errors.Is context.Canceled", runErr)
	}
}

// TestVisionRunner_RunVision_CtxDeadlineReturnsCtxErr is the deadline variant:
// an already-expired deadline must surface as context.DeadlineExceeded.
func TestVisionRunner_RunVision_CtxDeadlineReturnsCtxErr(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	bin := filepath.Join(dir, "fake-claude-vision-sleep2")
	script := "#!/bin/sh\nsleep 60\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}

	vr, err := NewVisionRunner(RunnerConfig{BinPath: bin, WorkDir: work})
	if err != nil {
		t.Fatalf("NewVisionRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1) // 1 ns — already expired
	defer cancel()

	_, runErr := vr.RunVision(ctx, []byte(`{}`+"\n"), "")
	if runErr == nil {
		t.Fatal("RunVision with expired deadline should return an error")
	}
	if !errors.Is(runErr, context.DeadlineExceeded) {
		t.Errorf("RunVision with expired deadline: got %v, want errors.Is context.DeadlineExceeded", runErr)
	}
}
