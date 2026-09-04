package sysession

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
)

// VisionRunner is an image-capable one-off claude invocation. Unlike Runner
// (text via stdin, --output-format text), a vision call ships an inline-base64
// image, which requires stream-json on BOTH input and output (the CLI rejects
// --input-format stream-json without a matching output format). It reuses
// NewRunner's env-filtering + binpath-hardening, so no IM tokens / dashboard
// secrets / cross-backend creds leak into the call. Kept a separate interface
// so the text-only daemon contract is unaffected.
type VisionRunner interface {
	// RunVision pipes one newline-terminated stream-json NDJSON user message
	// to `claude -p --input-format stream-json --output-format stream-json
	// --verbose` and returns the raw NDJSON stdout. A non-empty model
	// overrides --model. Honours ctx.
	RunVision(ctx context.Context, stdinLine []byte, model string) ([]byte, error)
}

// NewVisionRunner builds an image-capable one-off runner through the same
// construction path as NewRunner. A nil error guarantees a usable VisionRunner.
func NewVisionRunner(cfg RunnerConfig) (VisionRunner, error) {
	r, err := NewRunner(cfg)
	if err != nil {
		return nil, err
	}
	impl, ok := r.(*runnerImpl)
	if !ok {
		// Unreachable today (NewRunner is the sole constructor); guards against a
		// future decorator that lacks RunVision.
		return nil, fmt.Errorf("sysession: vision runner needs the exec-based runner")
	}
	return impl, nil
}

// visionBaseArgs mirrors runnerImplBaseArgs for the stream-json path.
// --setting-sources "" keeps host hooks from re-entering; --verbose is
// required by the CLI when --output-format stream-json is combined with -p.
var visionBaseArgs = []string{
	"-p",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--verbose",
	"--setting-sources", "",
}

// visionStdoutCapBytes caps the stream-json transcript: 1 MiB is comfortably
// above a legitimate one-turn transcript with system/init/result framing (the
// text Runner's 64 KiB cap is too tight) while bounding a runaway CLI.
const visionStdoutCapBytes = 1 << 20

// RunVision implements VisionRunner on the exec-based runner, reusing r.env
// (filtered) and r.cfg.BinPath (hardened).
func (r *runnerImpl) RunVision(ctx context.Context, stdinLine []byte, model string) ([]byte, error) {
	args := append([]string(nil), visionBaseArgs...)
	if model != "" {
		args = append(args, "--model", model)
	}

	cmd := exec.CommandContext(ctx, r.cfg.BinPath, args...)
	cmd.Dir = r.cfg.WorkDir
	cmd.Stdin = bytes.NewReader(stdinLine)
	cmd.Env = r.env

	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, max: runnerStderrCapBytes}

	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, max: visionStdoutCapBytes}

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// Do NOT fold stderr into the error: vision stderr can echo the stdin
		// base64 image blob. Log basename + exit status only.
		slog.Warn("sysession: vision runner failed",
			"binary", filepath.Base(r.cfg.BinPath), "err", err)
		return nil, fmt.Errorf("sysession: %s vision call failed: %w",
			filepath.Base(r.cfg.BinPath), err)
	}
	return stdout.Bytes(), nil
}
