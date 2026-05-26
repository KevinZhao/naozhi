package transcribe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/naozhi/naozhi/internal/osutil"
)

// ErrFFmpegNotFound is returned when ffmpeg is not installed.
var ErrFFmpegNotFound = errors.New("ffmpeg not found in PATH; install with: dnf install -y ffmpeg")

// lookupFFmpeg resolves the ffmpeg binary on every call rather than caching
// the first PATH-resolved entry process-wide.
//
// R240-SEC-9 (#1050): the previous implementation used sync.Once to memoise
// the first exec.LookPath result for the lifetime of the process. That
// turned a one-shot startup-time PATH resolution into a permanent foot-gun:
//
//   - If a privileged operator updates PATH (e.g. installs ffmpeg into
//     /usr/local/bin after the first transcribe attempt failed) the running
//     naozhi keeps returning ErrFFmpegNotFound until restart.
//   - If an attacker can transiently inject a writable directory ahead of
//     the system ffmpeg in PATH at startup (PATH injection at the systemd
//     unit / shell rc level), the malicious binary is pinned for the
//     entire process lifetime even after the operator removes the rogue
//     entry.
//
// Re-resolving on each transcribe request keeps the PATH binding fresh.
// exec.LookPath is a stat per PATH entry — well under a millisecond on
// any reasonable system, and this code path is voice-message-rate
// (≤ a few requests/second per chat), nowhere near a hot loop. The
// sync.Once-saved syscalls were never measurably worth the cache hazard.
func lookupFFmpeg() (string, error) {
	return exec.LookPath("ffmpeg")
}

// pcmStream holds a running ffmpeg process that streams PCM output.
type pcmStream struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr cappedBuffer
}

// cappedBuffer wraps bytes.Buffer with a max-byte gate to bound memory used
// by ffmpeg stderr capture. R188-SEC-L3: without a cap a malicious audio file
// that triggers pathological ffmpeg stderr output could accumulate unbounded
// memory × transcribeSemCap concurrent instances.
type cappedBuffer struct {
	buf     bytes.Buffer
	dropped int
}

const ffmpegStderrCap = 64 * 1024

// ffmpegMaxDecodeSeconds caps the wall-clock decode duration ffmpeg will
// emit to stdout. Without this argv flag a crafted/malicious audio file can
// keep ffmpeg busy producing PCM for arbitrarily long, holding one of the
// transcribeSemCap=3 concurrency slots and starving legitimate transcribe
// requests. The outer ctx provides a fallback (DialOptions / handler ctx
// deadline) but is not always set tight enough — argv-side `-t` is the
// process-local backstop. 600s = 10 minutes is well above any IM voice
// message in the field (typical ≤ 60s, hard upstream cap is 300s on
// Feishu) so this is a pure abuse mitigation. R247-SEC-6.
const ffmpegMaxDecodeSeconds = "600"

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remain := ffmpegStderrCap - c.buf.Len()
	if remain <= 0 {
		c.dropped += len(p)
		return len(p), nil
	}
	if len(p) > remain {
		c.buf.Write(p[:remain])
		c.dropped += len(p) - remain
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string {
	if c.dropped == 0 {
		return c.buf.String()
	}
	return fmt.Sprintf("%s...[%d bytes dropped]", c.buf.String(), c.dropped)
}

// Read implements io.Reader, reading PCM data from ffmpeg stdout.
func (p *pcmStream) Read(buf []byte) (int, error) {
	return p.stdout.Read(buf)
}

// Wait waits for ffmpeg to finish and returns any error.
func (p *pcmStream) Wait() error {
	if err := p.cmd.Wait(); err != nil {
		// Sanitize ffmpeg stderr before it flows into slog/error chains.
		// A crafted audio file can trigger error output carrying C0/C1/
		// bidi bytes that corrupt structured log parsing or terminal
		// rendering.
		return fmt.Errorf("ffmpeg convert: %w (stderr: %s)", err,
			osutil.SanitizeForLog(p.stderr.String(), 4096))
	}
	return nil
}

// Close kills the ffmpeg process (if still running) and reaps it.
func (p *pcmStream) Close() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return p.Wait()
}

// startPCMStream starts ffmpeg converting audio data to PCM (16kHz mono s16le)
// and returns a stream that can be read concurrently while ffmpeg is still running.
func startPCMStream(ctx context.Context, data []byte) (*pcmStream, error) {
	path, err := lookupFFmpeg()
	if err != nil {
		return nil, fmt.Errorf("%w (lookup: %v)", ErrFFmpegNotFound, err)
	}

	cmd := exec.CommandContext(ctx, path,
		"-i", "pipe:0",
		"-t", ffmpegMaxDecodeSeconds, // R247-SEC-6: argv-side wall-clock cap
		"-ar", "16000",
		"-ac", "1",
		"-f", "s16le",
		"pipe:1",
	)
	setSysProcAttr(cmd)
	cmd.Stdin = bytes.NewReader(data)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}

	ps := &pcmStream{cmd: cmd, stdout: stdout}
	cmd.Stderr = &ps.stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}

	return ps, nil
}
