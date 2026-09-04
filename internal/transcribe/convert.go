package transcribe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/osutil"
)

// ErrFFmpegNotFound is returned when ffmpeg is not installed.
var ErrFFmpegNotFound = errors.New("ffmpeg not found in PATH; install with: dnf install -y ffmpeg")

// ffmpegPathEnv is the operator override for the ffmpeg binary: an absolute
// path that skips PATH resolution, removing the PATH-injection surface.
const ffmpegPathEnv = "NAOZHI_FFMPEG_PATH"

// lookupFFmpeg resolves ffmpeg on every call rather than caching: a cached
// lookup would pin a missing binary or a transiently injected rogue PATH entry
// for the process lifetime, and this path runs at voice-message rate (#1050).
// Order: NAOZHI_FFMPEG_PATH (a vanished override errors loudly rather than
// falling back), then exec.LookPath("ffmpeg").
func lookupFFmpeg() (string, error) {
	if override := os.Getenv(ffmpegPathEnv); override != "" {
		// A relative override would degrade to a PATH search inside LookPath.
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("%s=%q: must be an absolute path", ffmpegPathEnv, override)
		}
		// LookPath accepts absolute inputs and verifies the +x bit (Stat does not).
		if path, err := exec.LookPath(override); err == nil {
			return path, nil
		} else {
			return "", fmt.Errorf("%s=%q: %w", ffmpegPathEnv, override, err)
		}
	}
	return exec.LookPath("ffmpeg")
}

// pcmStream holds a running ffmpeg process that streams PCM output.
type pcmStream struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr cappedBuffer
}

// cappedBuffer bounds ffmpeg stderr capture so a malicious audio file cannot
// accumulate unbounded memory × transcribeSemCap concurrent instances.
type cappedBuffer struct {
	buf     bytes.Buffer
	dropped int
}

const ffmpegStderrCap = 64 * 1024

// ffmpegMaxDecodeSeconds caps the decode duration (argv `-t`) so a crafted
// file cannot hold a transcribeSemCap slot indefinitely; the outer ctx is not
// always tight enough. 600s is far above any IM voice message (Feishu cap 300s).
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
		// Sanitize stderr before it reaches slog/error chains: a crafted audio
		// file can emit C0/C1/bidi bytes that corrupt log parsing or terminals.
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
// and returns a stream that can be read while ffmpeg is still running.
func startPCMStream(ctx context.Context, data []byte) (*pcmStream, error) {
	path, err := lookupFFmpeg()
	if err != nil {
		return nil, fmt.Errorf("%w (lookup: %v)", ErrFFmpegNotFound, err)
	}

	cmd := exec.CommandContext(ctx, path,
		"-i", "pipe:0",
		"-t", ffmpegMaxDecodeSeconds, // argv-side wall-clock cap
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
