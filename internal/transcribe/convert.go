package transcribe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
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
	stderr bytes.Buffer
}

// Read implements io.Reader, reading PCM data from ffmpeg stdout.
func (p *pcmStream) Read(buf []byte) (int, error) {
	return p.stdout.Read(buf)
}

// Wait waits for ffmpeg to finish and returns any error.
func (p *pcmStream) Wait() error {
	if err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg convert: %w (stderr: %s)", err, p.stderr.String())
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
