package transcribe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// ErrFFmpegNotFound is returned when ffmpeg is not installed.
var ErrFFmpegNotFound = errors.New("ffmpeg not found in PATH; install with: dnf install -y ffmpeg")

// ConvertToOgg converts audio data to OGG_OPUS (16kHz mono) via ffmpeg stdin/stdout pipe.
func ConvertToOgg(ctx context.Context, data []byte) ([]byte, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, ErrFFmpegNotFound
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-i", "pipe:0", // read from stdin
		"-ar", "16000", // 16kHz sample rate
		"-ac", "1", // mono
		"-c:a", "libopus", // Opus codec
		"-f", "ogg", // OGG container
		"pipe:1", // write to stdout
	)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg convert: %w (stderr: %s)", err, stderr.String())
	}

	return stdout.Bytes(), nil
}
