package transcribe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/naozhi/naozhi/internal/osutil"
)

// ErrFFmpegNotFound is returned when ffmpeg is not installed.
var ErrFFmpegNotFound = errors.New("ffmpeg not found in PATH; install with: dnf install -y ffmpeg")

var (
	ffmpegOnce sync.Once
	ffmpegPath string
	ffmpegErr  error
)

func lookupFFmpeg() (string, error) {
	ffmpegOnce.Do(func() {
		ffmpegPath, ffmpegErr = exec.LookPath("ffmpeg")
	})
	return ffmpegPath, ffmpegErr
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
