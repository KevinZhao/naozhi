package meeting

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Auth is naozhi-audio's authorisation state: "authorized", "denied", "not_determined" or
// "unknown" for each of the two grants.
type Auth struct {
	Microphone   string `json:"microphone"`
	AudioCapture string `json:"audio_capture"`
}

// Ready reports whether both grants are in place.
func (a Auth) Ready() bool { return a.Microphone == "authorized" && a.AudioCapture == "authorized" }

// Event is one line naozhi-audio record prints.
type Event struct {
	Event    string  `json:"event"`
	Error    string  `json:"error,omitempty"`
	Reason   string  `json:"reason,omitempty"`
	Index    int     `json:"index,omitempty"`
	Next     int     `json:"next_index,omitempty"`
	Seconds  float64 `json:"seconds,omitempty"`
	Offset   float64 `json:"offset,omitempty"`
	Mic      *Level  `json:"mic,omitempty"`
	Remote   *Level  `json:"remote,omitempty"`
	Procs    []Proc  `json:"processes,omitempty"`
	Rate     float64 `json:"rate,omitempty"`
	MicDev   string  `json:"mic_device,omitempty"`
	Streams  []int   `json:"streams,omitempty"`
	Path     string  `json:"path,omitempty"`
	ExitCode int     `json:"-"`
}

// Level is one track's segment as recorded.
type Level struct {
	Path string  `json:"path"`
	Peak float64 `json:"peak"`
	RMS  float64 `json:"rms"`
}

// ExitInputChanged is naozhi-audio record's exit code when the default microphone changed: the
// segments so far are complete and the next part starts on the new device.
const ExitInputChanged = 75

// Helper runs naozhi-audio. Tests substitute a fake.
type Helper interface {
	Status(ctx context.Context) (Auth, error)
	Request(ctx context.Context) (Auth, error)
	// Watch prints process snapshots until ctx ends or the helper exits.
	Watch(ctx context.Context, snap func([]Proc)) error
	// Record records into dir until stop is closed; events get every line; the returned event
	// is the last one with ExitCode set.
	Record(ctx context.Context, dir string, bundles []string, startIndex int, stop <-chan struct{}, events func(Event)) (Event, error)
	// Encode writes an AAC copy of a WAV file.
	Encode(ctx context.Context, wav, m4a string) error
}

// ErrNoHelper is returned when naozhi-audio is not installed next to the gateway.
var ErrNoHelper = errors.New("没有找到 naozhi-audio（随 macOS 发布包安装在 naozhi 旁边）")

// ExecHelper runs the naozhi-audio binary at Path.
type ExecHelper struct {
	Path string
	// ModelsDir is where diarize keeps its CoreML models (fetched on first use).
	ModelsDir string
}

// HelperPath is naozhi-audio beside the running executable.
func HelperPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Join(filepath.Dir(exe), "naozhi-audio")
}

func (h ExecHelper) cmd(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if h.Path == "" {
		return nil, ErrNoHelper
	}
	if _, err := os.Stat(h.Path); err != nil {
		return nil, ErrNoHelper
	}
	c := exec.CommandContext(ctx, h.Path, args...)
	c.WaitDelay = 5 * time.Second
	return c, nil
}

func (h ExecHelper) auth(ctx context.Context, sub string, timeout time.Duration) (Auth, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := h.cmd(ctx, sub)
	if err != nil {
		return Auth{}, err
	}
	out, err := c.Output()
	var a Auth
	if jerr := lastJSON(out, &a); jerr != nil {
		if err == nil {
			err = jerr
		}
		return Auth{}, fmt.Errorf("naozhi-audio %s: %w", sub, err)
	}
	return a, nil
}

// Status never prompts.
func (h ExecHelper) Status(ctx context.Context) (Auth, error) {
	return h.auth(ctx, "status", 10*time.Second)
}

// Request shows the system prompts that are still undecided and waits for the answers.
func (h ExecHelper) Request(ctx context.Context) (Auth, error) {
	return h.auth(ctx, "request", 6*time.Minute)
}

func lastJSON(out []byte, v any) error {
	var last []byte
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if b := sc.Bytes(); len(b) > 0 && b[0] == '{' {
			last = append(last[:0], b...)
		}
	}
	if last == nil {
		return errors.New("no output")
	}
	return json.Unmarshal(last, v)
}

// Watch keeps stdin open (the helper exits when it closes) and decodes each snapshot.
func (h ExecHelper) Watch(ctx context.Context, snap func([]Proc)) error {
	c, err := h.cmd(ctx, "watch", "-interval", "2")
	if err != nil {
		return err
	}
	stdin, _ := c.StdinPipe()
	defer stdin.Close()
	out, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var line struct {
			Event string `json:"event"`
			Procs []Proc `json:"procs"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Event == "procs" {
			snap(line.Procs)
		}
	}
	return c.Wait()
}

// Record closes the helper's stdin on stop: it finishes the current segment and exits 0.
func (h ExecHelper) Record(ctx context.Context, dir string, bundles []string, startIndex int, stop <-chan struct{}, events func(Event)) (Event, error) {
	args := []string{"record", "-out", dir, "-segment", "30", "-start-index", strconv.Itoa(startIndex)}
	for _, b := range bundles {
		args = append(args, "-bundle", b)
	}
	// Not ctx: a kill loses the segment being written; stop closes stdin instead.
	c, err := h.cmd(context.WithoutCancel(ctx), args...)
	if err != nil {
		return Event{}, err
	}
	stdin, err := c.StdinPipe()
	if err != nil {
		return Event{}, err
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return Event{}, err
	}
	if err := c.Start(); err != nil {
		return Event{}, err
	}
	var once sync.Once
	closeIn := func() { once.Do(func() { stdin.Close() }) }
	done := make(chan struct{})
	go func() {
		select {
		case <-stop:
			closeIn()
		case <-done:
		}
	}()
	var last Event
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		var ev Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Event != "" {
			last = ev
			events(ev)
		}
	}
	close(done)
	closeIn()
	err = c.Wait()
	last.ExitCode = c.ProcessState.ExitCode()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		err = nil
	}
	return last, err
}

// Diarize runs naozhi-audio diarize on one track (FluidAudio's offline pipeline, CoreML).
func (h ExecHelper) Diarize(ctx context.Context, wav string) ([]Turn, error) {
	secs, _ := WAVSeconds(wav)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute+time.Duration(secs)*time.Second/2)
	defer cancel()
	out := strings.TrimSuffix(wav, ".wav") + ".diar.json"
	args := []string{"diarize", wav, out}
	if h.ModelsDir != "" {
		args = append(args, "-models", h.ModelsDir)
	}
	c, err := h.cmd(ctx, args...)
	if err != nil {
		return nil, err
	}
	if b, err := c.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("naozhi-audio diarize: %w: %s", err, tail(b))
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	var res struct {
		Turns []Turn `json:"turns"`
	}
	return res.Turns, json.Unmarshal(b, &res)
}

// Encode runs naozhi-audio encode.
func (h ExecHelper) Encode(ctx context.Context, wav, m4a string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	c, err := h.cmd(ctx, "encode", wav, m4a)
	if err != nil {
		return err
	}
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("naozhi-audio encode: %w: %s", err, tail(out))
	}
	return nil
}

func tail(b []byte) string {
	if len(b) > 300 {
		b = b[len(b)-300:]
	}
	return string(b)
}
