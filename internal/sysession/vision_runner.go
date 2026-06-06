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
// (text prompt via stdin, --output-format text), a vision call must ship an
// inline-base64 image, which requires stream-json on BOTH input and output
// (the CLI rejects --input-format stream-json without a matching output
// format). It is otherwise the same transient-subprocess model as Runner and
// reuses the identical env-filtering + binpath-hardening from NewRunner — so
// no IM tokens / dashboard secrets / cross-backend creds leak into the call.
//
// The single consumer today is image auto-orientation (internal/server). It
// is deliberately a separate interface from Runner so the text-only daemon
// contract (AutoTitler) is unaffected.
type VisionRunner interface {
	// RunVision pipes a pre-built stream-json NDJSON user message (one line,
	// newline-terminated) to `claude -p --input-format stream-json
	// --output-format stream-json --verbose` and returns the raw stdout
	// (the NDJSON transcript) for the caller to parse. `model` overrides
	// --model when non-empty; empty leaves the CLI's default (Haiku-class
	// on this deployment). Honours ctx for timeout/cancel.
	RunVision(ctx context.Context, stdinLine []byte, model string) ([]byte, error)
}

// NewVisionRunner builds an image-capable one-off runner. It shares the
// exact construction path (env filtering, PATH/binpath hardening, WorkDir
// validation) as NewRunner — only the returned interface differs. A nil
// error guarantees a usable VisionRunner.
func NewVisionRunner(cfg RunnerConfig) (VisionRunner, error) {
	r, err := NewRunner(cfg)
	if err != nil {
		return nil, err
	}
	impl, ok := r.(*runnerImpl)
	if !ok {
		// NewRunner is the sole constructor of the concrete type; this is
		// unreachable but guards against a future refactor returning a
		// decorator that doesn't carry RunVision.
		return nil, fmt.Errorf("sysession: vision runner needs the exec-based runner")
	}
	return impl, nil
}

// visionBaseArgs mirrors runnerImplBaseArgs but for the stream-json vision
// path. --setting-sources "" keeps host hooks from re-entering, exactly as
// the text Runner does. --verbose is required by the CLI when
// --output-format stream-json is combined with -p.
var visionBaseArgs = []string{
	"-p",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--verbose",
	"--setting-sources", "",
}

// visionStdoutCapBytes caps the stream-json transcript. The orientation
// answer is a single word, but a verbose stream-json transcript carries
// system/init/result framing; 1 MiB is comfortably above a legitimate
// one-turn transcript while still bounding a runaway CLI from OOMing the
// parent. (The text Runner's 64 KiB cap is too tight for stream-json
// framing, hence a separate, larger constant here.)
const visionStdoutCapBytes = 1 << 20

// RunVision implements VisionRunner on the same exec-based runner used for
// text daemons, reusing r.env (filtered) and r.cfg.BinPath (hardened).
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
		// Do NOT fold stderr into the error here: a vision stderr can echo
		// the stdin, which is a base64 image blob — huge and useless in a
		// log. Log a short basename + exit status only.
		slog.Warn("sysession: vision runner failed",
			"binary", filepath.Base(r.cfg.BinPath), "err", err)
		return nil, fmt.Errorf("sysession: %s vision call failed: %w",
			filepath.Base(r.cfg.BinPath), err)
	}
	return stdout.Bytes(), nil
}
