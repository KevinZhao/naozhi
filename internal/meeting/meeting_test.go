package meeting

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable wall clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// writeSegment writes a 16 kHz mono segment of n samples of value v.
func writeSegment(t *testing.T, path string, n int, v int16) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, n*2)
	for i := range n {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	if err := os.WriteFile(path, append(wavHeader(int64(len(b))), b...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeHelper stands in for naozhi-audio. Each Record call writes perPart segments, then either
// waits for stop (exit 0) or, while exits is not empty, exits with the next code.
type fakeHelper struct {
	t       *testing.T
	auth    Auth
	snaps   chan []Proc
	perPart int
	silent  bool
	mu      sync.Mutex
	exits   []int
	starts  []int
	records int
}

func (f *fakeHelper) Status(context.Context) (Auth, error)  { return f.auth, nil }
func (f *fakeHelper) Request(context.Context) (Auth, error) { return f.auth, nil }
func (f *fakeHelper) Watch(ctx context.Context, snap func([]Proc)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case p := <-f.snaps:
			snap(p)
		}
	}
}

func (f *fakeHelper) Record(_ context.Context, dir string, _ []string, start int, stop <-chan struct{}, events func(Event)) (Event, error) {
	f.mu.Lock()
	f.records++
	f.starts = append(f.starts, start)
	f.mu.Unlock()
	remote := int16(1000)
	if f.silent {
		remote = 0
	}
	for i := range f.perPart {
		idx := start + i
		mp := filepath.Join(dir, "mic", fmt.Sprintf("%06d.wav", idx))
		rp := filepath.Join(dir, "remote", fmt.Sprintf("%06d.wav", idx))
		writeSegment(f.t, mp, SampleRate, 500)
		writeSegment(f.t, rp, SampleRate, remote)
		events(Event{Event: "segment", Index: idx, Seconds: 1, Mic: &Level{Path: mp, Peak: 0.015}, Remote: &Level{Path: rp, Peak: float64(remote) / 32768}})
	}
	f.mu.Lock()
	if len(f.exits) > 0 {
		code := f.exits[0]
		f.exits = f.exits[1:]
		f.mu.Unlock()
		return Event{Event: "stopped", ExitCode: code}, nil
	}
	f.mu.Unlock()
	<-stop
	return Event{Event: "stopped", ExitCode: 0}, nil
}

func (f *fakeHelper) Encode(_ context.Context, wav, m4a string) error {
	return os.WriteFile(m4a, []byte("aac"), 0o600)
}

// fakeASR returns one segment per call, saying which track it heard.
type fakeASR struct {
	mu    sync.Mutex
	calls []string
}

func (a *fakeASR) Name() string { return "fake" }
func (a *fakeASR) Transcribe(_ context.Context, wav string, h Hints) ([]Segment, error) {
	a.mu.Lock()
	a.calls = append(a.calls, filepath.Base(wav)+"|"+h.Title)
	a.mu.Unlock()
	secs, err := WAVSeconds(wav)
	if err != nil {
		return nil, err
	}
	return []Segment{{Start: 0.5, End: secs, Text: strings.TrimSuffix(filepath.Base(wav), ".wav") + " 说了话"}}, nil
}

type fakeDiar struct{}

func (fakeDiar) Diarize(context.Context, string) ([]Turn, error) {
	return []Turn{{Start: 0, End: 100, Speaker: "SPK_7"}}, nil
}

type harness struct {
	m        *Manager
	h        *fakeHelper
	asr      *fakeASR
	clock    *fakeClock
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	notices  []string
	promises []string
	journal  []string
	prompts  []string
}

const summaryReply = "## 结论\n上线定在下周。\n\n```json\n{\"todos\":[{\"owner\":\"你\",\"text\":\"发报价单\",\"due\":\"2026-11-20\",\"mine\":true},{\"owner\":\"Bob\",\"text\":\"准备演示\",\"due\":\"\",\"mine\":false}]}\n```\n"

func newHarness(t *testing.T, mut func(*Config, *fakeHelper)) *harness {
	t.Helper()
	x := &harness{
		h:     &fakeHelper{t: t, auth: Auth{Microphone: "authorized", AudioCapture: "authorized"}, snaps: make(chan []Proc), perPart: 3},
		asr:   &fakeASR{},
		clock: &fakeClock{t: time.Date(2026, 11, 9, 10, 0, 0, 0, time.Local)},
		done:  make(chan struct{}),
	}
	cfg := Config{
		Root:       t.TempDir(),
		Helper:     x.h,
		Recognizer: x.asr,
		Diarizer:   fakeDiar{},
		Summarize: func(_ context.Context, key, dir, text string) (string, error) {
			x.mu.Lock()
			x.prompts = append(x.prompts, key+"\n"+text)
			x.mu.Unlock()
			return summaryReply, nil
		},
		Calendar: func() []CalendarEvent {
			return []CalendarEvent{{Title: "客户 A 周会", Start: x.clock.Now().Add(-2 * time.Minute), End: x.clock.Now().Add(time.Hour), Attendees: []string{"Bob"}}}
		},
		Promise: func(text string, due time.Time) (string, error) {
			x.mu.Lock()
			defer x.mu.Unlock()
			x.promises = append(x.promises, text+"@"+due.Format("01-02 15:04"))
			return fmt.Sprintf("p%d", len(x.promises)), nil
		},
		Notify: func(key, title, body string) {
			x.mu.Lock()
			x.notices = append(x.notices, title+"|"+body)
			x.mu.Unlock()
		},
		Journal: func(d map[string]any) {
			x.mu.Lock()
			x.journal = append(x.journal, fmt.Sprint(d["step"]))
			x.mu.Unlock()
		},
		Now: x.clock.Now,
	}
	if mut != nil {
		mut(&cfg, x.h)
	}
	x.m = New(cfg)
	var ctx context.Context
	ctx, x.cancel = context.WithCancel(context.Background())
	go func() { x.m.Run(ctx); close(x.done) }()
	t.Cleanup(x.stop)
	return x
}

func (x *harness) stop() {
	x.cancel()
	<-x.done
}

func (x *harness) snap(p ...Proc) { x.h.snaps <- p }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var teams = Proc{PID: 100, Bundle: "com.microsoft.teams2.modulehost", Input: true, Output: true}

func (x *harness) only(t *testing.T) Meeting {
	t.Helper()
	ms := x.m.Store().List()
	if len(ms) != 1 {
		t.Fatalf("meetings = %d, want 1", len(ms))
	}
	return ms[0]
}

func TestDetector(t *testing.T) {
	var d Detector
	now := time.Now()
	own := Proc{Bundle: HelperBundle, Input: true}
	if a := d.Observe(now, []Proc{own, {Bundle: "com.microsoft.teams2", Output: true}}); a != ActNone {
		t.Fatalf("output only / own helper started a meeting: %v", a)
	}
	if a := d.Observe(now, []Proc{teams}); a != ActStart || d.App().Name != "Teams" {
		t.Fatalf("mic in use: %v %v", a, d.App())
	}
	if a := d.Observe(now.Add(Grace-time.Second), nil); a != ActNone {
		t.Fatalf("stopped within grace: %v", a)
	}
	if a := d.Observe(now.Add(Grace+time.Second), []Proc{teams}); a != ActNone {
		t.Fatalf("mic back: %v", a)
	}
	if a := d.Observe(now.Add(2*Grace+2*time.Second), nil); a != ActStop || d.App() != nil {
		t.Fatalf("after grace: %v", a)
	}
	// A declined meeting stays off until the program leaves the mic, then the next one starts.
	d.Observe(now, []Proc{teams})
	d.Decline()
	if a := d.Observe(now.Add(time.Minute), []Proc{teams}); a != ActNone || !d.Declined() {
		t.Fatalf("declined restarted: %v", a)
	}
	if a := d.Observe(now.Add(time.Minute+Grace), nil); a != ActNone {
		t.Fatalf("declined meeting reported stop: %v", a)
	}
	if a := d.Observe(now.Add(time.Hour), []Proc{{Bundle: "com.google.Chrome.helper", Input: true}}); a != ActStart || d.App().Name != "Chrome" {
		t.Fatalf("browser meeting: %v", a)
	}
}

func TestRecordSummarizeEndToEnd(t *testing.T) {
	x := newHarness(t, nil)
	x.snap(teams)
	waitFor(t, "recording", func() bool { st := x.m.Status(); return st.Recording != nil && st.Recording.Segments == 3 })
	st := x.m.Status()
	if st.Recording.Title != "客户 A 周会" || st.Recording.App != "Teams" {
		t.Fatalf("recording = %+v", st.Recording)
	}
	// The program releases the mic; nothing happens within the grace, then the meeting ends.
	x.snap()
	x.clock.Add(Grace + time.Second)
	x.snap()
	waitFor(t, "done", func() bool { ms := x.m.Store().List(); return len(ms) == 1 && ms[0].State == StateDone })
	mt := x.only(t)
	if mt.Seconds != 3 || mt.StopBy != "auto" || mt.Silent || mt.Speakers != 1 {
		t.Fatalf("meeting = %+v", mt)
	}
	dir := x.m.Store().Dir(mt.ID)
	md, err := os.ReadFile(filepath.Join(dir, "transcript.md"))
	if err != nil || !strings.Contains(string(md), "**你**") || !strings.Contains(string(md), "**说话人 A**") {
		t.Fatalf("transcript.md: %v\n%s", err, md)
	}
	sum, err := os.ReadFile(filepath.Join(dir, "summary.md"))
	if err != nil || !strings.Contains(string(sum), "上线定在下周") || strings.Contains(string(sum), "```json") {
		t.Fatalf("summary.md: %v\n%s", err, sum)
	}
	if len(mt.Todos) != 2 || mt.Todos[0].Promise != "p1" || mt.Todos[1].Promise != "" {
		t.Fatalf("todos = %+v", mt.Todos)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.promises) != 1 || x.promises[0] != "发报价单（会议「客户 A 周会」）@11-20 09:00" {
		t.Fatalf("promises = %q", x.promises)
	}
	if len(x.notices) != 1 || !strings.Contains(x.notices[0], "纪要好了") || !strings.Contains(x.notices[0], "你的 1 条") {
		t.Fatalf("notices = %q", x.notices)
	}
	if !strings.HasPrefix(x.prompts[0], SummaryKeyPrefix+mt.ID) || !strings.Contains(x.prompts[0], "你：mic 说了话") || !strings.Contains(x.prompts[0], "Bob") {
		t.Fatalf("prompt = %s", x.prompts[0])
	}
	if got := strings.Join(x.journal, ","); got != "record_start,record_stop,transcript,summary" {
		t.Fatalf("journal = %s", got)
	}
	// The WAV segments gave way to the AAC copies, kept for 30 days.
	if _, err := os.Stat(filepath.Join(dir, "audio", "mic")); !os.IsNotExist(err) {
		t.Fatalf("segments kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audio", "remote.m4a")); err != nil {
		t.Fatal(err)
	}
	if !mt.AudioExpires.Equal(x.clock.Now().Add(AudioRetention)) {
		t.Fatalf("expires %v", mt.AudioExpires)
	}
}

func TestUserStopHoldsUntilMicReleased(t *testing.T) {
	x := newHarness(t, nil)
	x.snap(teams)
	waitFor(t, "recording", func() bool { return x.m.Status().Recording != nil })
	if !x.m.StopRecording("user") {
		t.Fatal("nothing to stop")
	}
	if x.m.Status().Recording != nil {
		t.Fatal("still recording")
	}
	x.snap(teams)
	x.clock.Add(time.Minute)
	x.snap(teams)
	if x.m.Status().Recording != nil || x.h.records != 1 {
		t.Fatalf("restarted after the user stopped it (%d records)", x.h.records)
	}
	waitFor(t, "processed", func() bool { return x.only(t).State == StateDone })
	if x.only(t).StopBy != "user" {
		t.Fatalf("stop_by = %s", x.only(t).StopBy)
	}
}

func TestRestartContinuesSegments(t *testing.T) {
	x := newHarness(t, func(_ *Config, h *fakeHelper) { h.exits = []int{ExitInputChanged, 1} })
	x.snap(teams)
	waitFor(t, "three parts", func() bool { r := x.m.Status().Recording; return r != nil && r.Segments == 9 })
	x.h.mu.Lock()
	starts := fmt.Sprint(x.h.starts)
	x.h.mu.Unlock()
	if starts != "[1 4 7]" {
		t.Fatalf("start indexes %s", starts)
	}
	x.m.StopRecording("user")
	waitFor(t, "processed", func() bool { return x.only(t).State == StateDone })
	if mt := x.only(t); mt.Parts != 2 || mt.Seconds != 9 {
		t.Fatalf("meeting = %+v", mt)
	}
}

func TestHelperFailingWithoutSegmentsGivesUp(t *testing.T) {
	x := newHarness(t, func(_ *Config, h *fakeHelper) { h.perPart = 0; h.exits = []int{77} })
	x.snap(teams)
	waitFor(t, "failed", func() bool { ms := x.m.Store().List(); return len(ms) == 1 && ms[0].State == StateFailed })
	if x.m.Status().Recording != nil {
		t.Fatal("still recording")
	}
}

func TestUnauthorisedDoesNotRecord(t *testing.T) {
	x := newHarness(t, func(_ *Config, h *fakeHelper) {
		h.auth = Auth{Microphone: "authorized", AudioCapture: "not_determined"}
	})
	x.snap(teams)
	x.snap(teams)
	if len(x.m.Store().List()) != 0 {
		t.Fatal("recorded without authorisation")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.notices) != 1 || !strings.Contains(x.notices[0], "还没有录音授权") {
		t.Fatalf("notices = %q", x.notices)
	}
}

func TestSilentFarEndIsFlagged(t *testing.T) {
	x := newHarness(t, func(_ *Config, h *fakeHelper) { h.silent = true })
	x.snap(teams)
	waitFor(t, "recording", func() bool { r := x.m.Status().Recording; return r != nil && r.Segments == 3 })
	x.m.StopRecording("user")
	waitFor(t, "processed", func() bool { return x.only(t).State == StateDone })
	mt := x.only(t)
	if !mt.Silent || mt.MicSilent {
		t.Fatalf("silent=%v mic_silent=%v", mt.Silent, mt.MicSilent)
	}
	x.asr.mu.Lock()
	calls := len(x.asr.calls)
	x.asr.mu.Unlock()
	if calls != 1 {
		t.Fatalf("silent far end was transcribed (%d calls)", calls)
	}
}

func TestShutdownResumesSameMeeting(t *testing.T) {
	root := t.TempDir()
	x := newHarness(t, func(c *Config, _ *fakeHelper) { c.Root = root })
	x.snap(teams)
	waitFor(t, "recording", func() bool { r := x.m.Status().Recording; return r != nil && r.Segments == 3 })
	x.stop()
	if mt := x.only(t); mt.State != StateRecording {
		t.Fatalf("state after shutdown %s", mt.State)
	}
	y := newHarness(t, func(c *Config, _ *fakeHelper) { c.Root = root })
	y.snap(teams)
	waitFor(t, "resumed", func() bool { r := y.m.Status().Recording; return r != nil && r.Segments == 6 })
	y.h.mu.Lock()
	start := y.h.starts[0]
	y.h.mu.Unlock()
	if start != 4 {
		t.Fatalf("resumed at index %d", start)
	}
	if n := len(y.m.Store().List()); n != 1 {
		t.Fatalf("meetings = %d", n)
	}
}

func TestOrphanProcessedAfterAge(t *testing.T) {
	root := t.TempDir()
	x := newHarness(t, func(c *Config, _ *fakeHelper) { c.Root = root })
	x.snap(teams)
	waitFor(t, "recording", func() bool { r := x.m.Status().Recording; return r != nil && r.Segments == 3 })
	x.stop()
	mt := x.only(t)
	clock := &fakeClock{t: mt.LastSegment.Add(orphanAge)}
	y := newHarness(t, func(c *Config, _ *fakeHelper) { c.Root = root; c.Now = clock.Now })
	waitFor(t, "processed", func() bool { return y.only(t).State == StateDone })
	if y.only(t).StopBy != "restart" {
		t.Fatalf("stop_by = %s", y.only(t).StopBy)
	}
}

func TestAudioRetention(t *testing.T) {
	x := newHarness(t, nil)
	x.snap(teams)
	waitFor(t, "recording", func() bool { r := x.m.Status().Recording; return r != nil && r.Segments == 3 })
	x.m.StopRecording("user")
	waitFor(t, "processed", func() bool { return x.only(t).State == StateDone })
	id := x.only(t).ID
	if _, err := x.m.KeepAudio(id, true); err != nil {
		t.Fatal(err)
	}
	x.clock.Add(AudioRetention + time.Hour)
	x.m.sweepAudio()
	if x.only(t).AudioDeleted {
		t.Fatal("kept audio deleted")
	}
	x.m.KeepAudio(id, false)
	x.m.sweepAudio()
	if !x.only(t).AudioDeleted {
		t.Fatal("expired audio kept")
	}
	if _, err := os.Stat(filepath.Join(x.m.Store().Dir(id), "audio")); !os.IsNotExist(err) {
		t.Fatalf("audio dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(x.m.Store().Dir(id), "summary.md")); err != nil {
		t.Fatal("summary went with the audio")
	}
}

func TestSummaryFailureThenRetry(t *testing.T) {
	var fail = true
	var mu sync.Mutex
	x := newHarness(t, func(c *Config, _ *fakeHelper) {
		c.Summarize = func(context.Context, string, string, string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if fail {
				return "", errors.New("model down")
			}
			return "纪要正文", nil
		}
	})
	x.snap(teams)
	waitFor(t, "recording", func() bool { r := x.m.Status().Recording; return r != nil && r.Segments == 3 })
	x.m.StopRecording("user")
	waitFor(t, "failed", func() bool { return x.only(t).State == StateFailed })
	if !strings.Contains(x.only(t).Error, "model down") {
		t.Fatalf("error = %s", x.only(t).Error)
	}
	x.asr.mu.Lock()
	before := len(x.asr.calls)
	x.asr.mu.Unlock()
	mu.Lock()
	fail = false
	mu.Unlock()
	if err := x.m.Retry(x.only(t).ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "done", func() bool { return x.only(t).State == StateDone })
	x.asr.mu.Lock()
	after := len(x.asr.calls)
	x.asr.mu.Unlock()
	if after != before {
		t.Fatalf("retry transcribed again (%d → %d)", before, after)
	}
}

func TestConcatPadsMissingSegment(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav")
	writeSegment(t, a, 8000, 1)
	writeSegment(t, b, 16000, 2)
	secs, err := ConcatWAV(filepath.Join(dir, "out.wav"), []string{a, filepath.Join(dir, "missing.wav"), b}, []float64{0, 0.25, 0})
	if err != nil || secs != 1.75 {
		t.Fatalf("secs=%v err=%v", secs, err)
	}
	if got, _ := WAVSeconds(filepath.Join(dir, "out.wav")); got != 1.75 {
		t.Fatalf("WAVSeconds = %v", got)
	}
}

func TestParseSummary(t *testing.T) {
	md, todos := parseSummary(summaryReply)
	if strings.Contains(md, "json") || len(todos) != 2 || !todos[0].Mine || todos[1].Mine {
		t.Fatalf("md=%q todos=%+v", md, todos)
	}
	md, todos = parseSummary("只有正文")
	if md != "只有正文" || todos != nil {
		t.Fatalf("plain: %q %v", md, todos)
	}
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.Local)
	if d := dueOf("2026-11-08", now); !d.IsZero() {
		t.Fatalf("past due kept: %v", d)
	}
}
