package meeting

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed engine/naozhi_meeting_engine.py
var engineScript []byte

// ErrNoUV is returned when uv, which runs the local recogniser, is not installed.
var ErrNoUV = errors.New("本机识别需要 uv（brew install uv），没有找到")

// LocalEngine is the local recogniser (MR3): engine/naozhi_meeting_engine.py run with
// `uv run --script`, which keeps its own environment (MLX, Qwen3-ASR, Silero VAD) and fetches
// the model on first use. Nothing leaves the machine.
type LocalEngine struct {
	// Dir is where the script is written (Dir/.engine).
	Dir string
	// UV is uv's path; "" looks it up.
	UV string
	// Env is added to the script's environment (tests, HF_HOME).
	Env []string
}

// Name names the engine in transcript.json.
func (e *LocalEngine) Name() string { return "qwen3-asr-1.7b-mlx" }

// uvCandidates is where uv lives when the gateway's PATH (launchd's) lacks it.
var uvCandidates = []string{"/opt/homebrew/bin/uv", "/usr/local/bin/uv", "~/.local/bin/uv", "~/.cargo/bin/uv"}

// FindUV is uv's path, "" when it is not installed.
func FindUV() string {
	if p, err := exec.LookPath("uv"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, c := range uvCandidates {
		c = strings.Replace(c, "~", home, 1)
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

func (e *LocalEngine) script() (string, error) {
	dir := filepath.Join(e.Dir, ".engine")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "naozhi_meeting_engine.py")
	if b, err := os.ReadFile(p); err == nil && bytes.Equal(b, engineScript) {
		return p, nil
	}
	return p, writeFile(p, engineScript)
}

// Transcribe runs the script's transcribe on one track. The first run downloads the
// environment and the model (a few GB), so the timeout is generous.
func (e *LocalEngine) Transcribe(ctx context.Context, wav string, h Hints) ([]Segment, error) {
	uv := e.UV
	if uv == "" {
		uv = FindUV()
	}
	if uv == "" {
		return nil, ErrNoUV
	}
	script, err := e.script()
	if err != nil {
		return nil, err
	}
	secs, _ := WAVSeconds(wav)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute+time.Duration(secs)*time.Second)
	defer cancel()
	out := strings.TrimSuffix(wav, ".wav") + ".asr.json"
	args := []string{"run", "--quiet", "--script", script, "transcribe", "--input", wav, "--output", out}
	if c := hintText(h); c != "" {
		args = append(args, "--context", c)
	}
	cmd := exec.CommandContext(ctx, uv, args...)
	cmd.Env = append(os.Environ(), e.Env...)
	cmd.WaitDelay = 10 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("识别引擎：%w: %s", err, tail(stderr.Bytes()))
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	var res struct {
		Segments []Segment `json:"segments"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	return res.Segments, nil
}

// hintText is the recogniser's context prompt: names and terms it should spell right.
func hintText(h Hints) string {
	var parts []string
	if h.Title != "" {
		parts = append(parts, "会议："+h.Title)
	}
	if len(h.Attendees) > 0 {
		parts = append(parts, "参会人："+strings.Join(h.Attendees, "、"))
	}
	return strings.Join(parts, "\n")
}
