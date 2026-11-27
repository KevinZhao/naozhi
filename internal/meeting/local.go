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
	"unicode"
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
	if h.Language != "" {
		cmd.Env = append(cmd.Env, "NAOZHI_MEETING_ASR_LANGUAGE="+h.Language)
	}
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
	return dropHintEchoes(res.Segments, hintText(h)), nil
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
	if len(h.Terms) > 0 {
		parts = append(parts, "可能出现的项目与术语："+strings.Join(h.Terms[:min(len(h.Terms), maxTerms)], "、"))
	}
	return strings.Join(parts, "\n")
}

// hintLabels are the labels hintText writes; an echo carries one of them.
var hintLabels = []string{"会议", "参会人", "可能出现的项目与术语"}

// dropHintEchoes drops the segments that only repeat the context prompt: over a nearly silent
// stretch the recogniser writes out its context ("会议：<标题>。参会人：…") as if someone said
// it. A segment is an echo when, punctuation and spaces aside, it is a piece of the prompt
// that carries one of its labels; speech that merely names the meeting is kept.
func dropHintEchoes(segs []Segment, hint string) []Segment {
	h := echoKey(hint)
	if h == "" {
		return segs
	}
	out := segs[:0]
	for _, s := range segs {
		k := echoKey(s.Text)
		if k != "" && strings.Contains(h, k) && hasHintLabel(k) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func hasHintLabel(k string) bool {
	for _, l := range hintLabels {
		if strings.Contains(k, l) {
			return true
		}
	}
	return false
}

// echoKey is s with only its letters and digits, lower-cased.
func echoKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
