// Package meeting records online meetings on this Mac and turns them into transcripts, summaries
// and to-dos (docs/rfc/meeting-notes.md, M1).
//
// naozhi-audio (tools/naozhi-audio) does all Core Audio work; this package follows its process
// snapshots, starts a recording when a meeting program takes the microphone (MR1), keeps the
// 30-second segments it writes, and after the meeting runs recognition, speaker separation and a
// one-shot summary session. Everything a meeting produces lives in its own directory under the
// state root.
package meeting

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// UnitID is the meeting recorder's unit in the journal.
const UnitID = "sys:meeting"

// SummaryKeyPrefix is the namespace of the summary sessions; the key is the prefix plus the
// meeting ID.
const SummaryKeyPrefix = "meeting:"

const (
	// silentPeak is the peak below which a track counts as silence (16-bit LSB ≈ 3e-5).
	silentPeak = 1e-4
	// maxRestarts bounds how often one recording restarts the helper without a new segment.
	maxRestarts = 5
	// orphanAge is how long a recording left by a restarted gateway waits to be resumed by the
	// same meeting before it is processed as ended.
	orphanAge = 10 * time.Minute
	// authEvery is how often the authorisation state is re-read.
	authEvery = 10 * time.Minute
	// summaryTimeout bounds the summary session.
	summaryTimeout = 15 * time.Minute
)

// Session runs the one-shot summary session key in dir with text and returns its reply; the
// session is gone when it returns.
type Session func(ctx context.Context, key, dir, text string) (string, error)

// Config wires a Manager. Only Root and Helper are required.
type Config struct {
	// Root is the meetings directory (<state root>/meetings).
	Root       string
	Helper     Helper
	Recognizer Recognizer
	Diarizer   Diarizer
	Summarize  Session
	// Calendar is the cached calendar (the scout's); nil or empty names meetings by program.
	Calendar func() []CalendarEvent
	// Promise registers one of the user's to-dos as a promise and returns its ID.
	Promise func(text string, due time.Time) (string, error)
	// Notify pushes a notice; key ties repeats of one notice together.
	Notify func(key, title, body string)
	// Journal records a step as an activity entry.
	Journal func(data map[string]any)
	// Changed is told after every change of Status (the dashboard's red strip).
	Changed func(Status)
	Now     func() time.Time
}

// Status is the recorder as the dashboard sees it.
type Status struct {
	// Available is set when naozhi-audio is installed and the platform is macOS.
	Available bool `json:"available"`
	Auth      Auth `json:"auth"`
	// Recording is the meeting being recorded, nil when none.
	Recording *Meeting `json:"recording,omitempty"`
	// Processing is the meeting whose transcript or summary is being made.
	Processing string `json:"processing,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Manager owns detection, recording and processing.
type Manager struct {
	cfg   Config
	store *Store

	mu      sync.Mutex
	det     Detector
	auth    Auth
	authAt  time.Time
	authErr string
	rec     *recording
	// orphans are recordings a previous run of the gateway left in StateRecording.
	orphans    []Meeting
	processing string
	queue      chan string
	wg         sync.WaitGroup
	// unauthorisedTold is the day the "not authorised" notice went out.
	unauthorisedTold string
}

type recording struct {
	m       Meeting
	stop    chan struct{}
	stopped chan struct{}
	by      string
	heard   bool
	micOn   bool
}

// New loads the meetings directory. It starts nothing: Run does.
func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{cfg: cfg, store: &Store{Root: cfg.Root}, queue: make(chan string, 64)}
}

// Store is the meetings directory.
func (m *Manager) Store() *Store { return m.store }

func (m *Manager) available() bool { return m != nil && m.cfg.Helper != nil && m.cfg.Root != "" }

// Run follows the meeting programs until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	if !m.available() {
		return
	}
	m.refreshAuth(ctx)
	for _, mt := range m.store.List() {
		switch mt.State {
		case StateRecording:
			m.mu.Lock()
			m.orphans = append(m.orphans, mt)
			m.mu.Unlock()
		case StateProcessing:
			m.enqueue(mt.ID)
		}
	}
	m.wg.Add(3)
	go func() { defer m.wg.Done(); m.processLoop(ctx) }()
	// The helper is stopped through its stdin, never killed with ctx: it then closes the
	// segment it is writing, and the next run resumes the meeting.
	go func() { defer m.wg.Done(); <-ctx.Done(); m.StopRecording("shutdown") }()
	go func() { defer m.wg.Done(); m.housekeeping(ctx) }()
	backoff := time.Second
	for ctx.Err() == nil {
		start := m.cfg.Now()
		err := m.cfg.Helper.Watch(ctx, func(p []Proc) { m.observe(ctx, p) })
		if ctx.Err() != nil {
			break
		}
		if errors.Is(err, ErrNoHelper) {
			m.setError(err.Error())
			return
		}
		slog.Warn("meeting: naozhi-audio watch ended", "err", err)
		if m.cfg.Now().Sub(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	m.wg.Wait()
}

func (m *Manager) setError(s string) {
	m.mu.Lock()
	m.authErr = s
	m.mu.Unlock()
	m.changed()
}

func (m *Manager) refreshAuth(ctx context.Context) Auth {
	a, err := m.cfg.Helper.Status(ctx)
	m.mu.Lock()
	m.auth, m.authAt = a, m.cfg.Now()
	m.authErr = ""
	if err != nil {
		m.authErr = err.Error()
	}
	m.mu.Unlock()
	return a
}

// Authorize shows the system prompts for whatever is still undecided.
func (m *Manager) Authorize(ctx context.Context) (Auth, error) {
	if !m.available() {
		return Auth{}, ErrNoHelper
	}
	a, err := m.cfg.Helper.Request(ctx)
	if err != nil {
		return a, err
	}
	m.mu.Lock()
	m.auth, m.authAt, m.authErr = a, m.cfg.Now(), ""
	m.mu.Unlock()
	m.changed()
	return a, nil
}

// Status reports the recorder.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Available: m.available(), Auth: m.auth, Processing: m.processing, Error: m.authErr}
	if m.rec != nil {
		r := m.rec.m
		st.Recording = &r
	}
	return st
}

// SetChanged sets the status exit (the dashboard's frame), bound once the server exists.
func (m *Manager) SetChanged(fn func(Status)) {
	m.mu.Lock()
	m.cfg.Changed = fn
	m.mu.Unlock()
}

func (m *Manager) changed() {
	m.mu.Lock()
	fn := m.cfg.Changed
	m.mu.Unlock()
	if fn != nil {
		fn(m.Status())
	}
}

func (m *Manager) journal(data map[string]any) {
	if m.cfg.Journal != nil {
		data["type"] = "meeting"
		m.cfg.Journal(data)
	}
}

func (m *Manager) observe(ctx context.Context, procs []Proc) {
	now := m.cfg.Now()
	m.mu.Lock()
	act := m.det.Observe(now, procs)
	app := m.det.App()
	auth := m.auth
	m.mu.Unlock()
	switch act {
	case ActStart:
		if !auth.Ready() && now.Sub(m.authAt) > time.Minute {
			auth = m.refreshAuth(ctx)
		}
		if !auth.Ready() {
			m.tellUnauthorised(*app, now)
			m.mu.Lock()
			m.det.Decline()
			m.mu.Unlock()
			return
		}
		m.startRecording(ctx, *app, "auto")
	case ActStop:
		m.StopRecording("auto")
	}
}

// tellUnauthorised pushes, once a day, that a meeting was not recorded for want of the grants:
// recording without them would bring the system prompts up in the middle of the meeting.
func (m *Manager) tellUnauthorised(app App, now time.Time) {
	day := now.Format("2006-01-02")
	m.mu.Lock()
	told := m.unauthorisedTold == day
	m.unauthorisedTold = day
	m.mu.Unlock()
	if told || m.cfg.Notify == nil {
		return
	}
	m.cfg.Notify("meeting:auth", "检测到 "+app.Name+" 会议，但还没有录音授权",
		"在 dashboard 的 设置 → 会议录音 里授权「麦克风」和「屏幕与系统录音」后，下一场会议会自动录音。")
}

func (m *Manager) startRecording(ctx context.Context, app App, by string) {
	now := m.cfg.Now()
	m.mu.Lock()
	if m.rec != nil {
		m.mu.Unlock()
		return
	}
	// A recording the last run of the gateway left behind continues when the same program is
	// still (or again) in a meeting.
	var mt Meeting
	resumed := false
	for i, o := range m.orphans {
		if o.App == app.Name && now.Sub(lastActive(o)) < orphanAge {
			mt, resumed = o, true
			m.orphans = append(m.orphans[:i], m.orphans[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	if !resumed {
		mt = Meeting{App: app.Name, Bundles: app.Bundles, Started: now, State: StateRecording,
			Title: fmt.Sprintf("%s 会议 %s", app.Name, now.Local().Format("15:04"))}
		if m.cfg.Calendar != nil {
			if e, ok := eventAt(m.cfg.Calendar(), now); ok {
				mt.Title, mt.Attendees = e.Title, e.Attendees
			}
		}
		var err error
		if mt, err = m.store.Create(mt); err != nil {
			slog.Error("meeting: create", "err", err)
			return
		}
	}
	r := &recording{m: mt, stop: make(chan struct{}), stopped: make(chan struct{})}
	m.mu.Lock()
	m.rec = r
	m.mu.Unlock()
	m.journal(map[string]any{"meeting": mt.ID, "step": "record_start", "summary": "开始录音：" + mt.Title, "app": app.Name, "by": by, "resumed": resumed})
	m.changed()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.record(ctx, r)
	}()
}

func lastActive(mt Meeting) time.Time {
	if mt.LastSegment.After(mt.Started) {
		return mt.LastSegment
	}
	return mt.Started
}

// record runs naozhi-audio until the recording is stopped, restarting it when the microphone
// changes or the helper dies; each restart continues the segment numbering.
func (m *Manager) record(ctx context.Context, r *recording) {
	defer close(r.stopped)
	dir := filepath.Join(m.store.Dir(r.m.ID), "audio")
	restarts := 0
	var lastErr string
	for {
		next := nextIndex(m.store, r.m.ID)
		before := next
		ev, err := m.cfg.Helper.Record(ctx, dir, r.m.Bundles, next, r.stop, func(ev Event) { m.onEvent(r, ev) })
		select {
		case <-r.stop:
			m.finishRecording(r, "")
			return
		default:
		}
		if ctx.Err() != nil {
			m.mu.Lock()
			r.by = cmp.Or(r.by, "shutdown")
			m.mu.Unlock()
			m.finishRecording(r, "")
			return
		}
		switch {
		case err != nil:
			lastErr = err.Error()
		case ev.ExitCode == ExitInputChanged:
			lastErr = ""
		case ev.Event == "error":
			lastErr = ev.Error
		default:
			lastErr = fmt.Sprintf("naozhi-audio exited %d", ev.ExitCode)
		}
		if nextIndex(m.store, r.m.ID) > before {
			restarts = 0
		} else {
			restarts++
		}
		if restarts > maxRestarts || ev.ExitCode == 77 || errors.Is(err, ErrNoHelper) {
			m.finishRecording(r, lastErr)
			return
		}
		m.store.Update(r.m.ID, func(mt *Meeting) { mt.Parts++ })
		slog.Info("meeting: recorder restarting", "meeting", r.m.ID, "exit", ev.ExitCode, "err", lastErr)
		select {
		case <-r.stop:
			m.finishRecording(r, "")
			return
		case <-time.After(time.Second):
		}
	}
}

func nextIndex(s *Store, id string) int {
	n := 0
	for _, track := range []string{"mic", "remote"} {
		for _, p := range s.segments(id, track) {
			var i int
			if _, err := fmt.Sscanf(filepath.Base(p), "%06d.wav", &i); err == nil && i > n {
				n = i
			}
		}
	}
	return n + 1
}

func (m *Manager) onEvent(r *recording, ev Event) {
	if ev.Event != "segment" {
		return
	}
	heard := ev.Remote != nil && ev.Remote.Peak > silentPeak
	micOn := ev.Mic != nil && ev.Mic.Peak > silentPeak
	now := m.cfg.Now()
	mt, err := m.store.Update(r.m.ID, func(mt *Meeting) {
		mt.Segments++
		mt.Seconds += ev.Seconds
		mt.LastSegment = now
	})
	if err != nil {
		return
	}
	m.mu.Lock()
	r.m = mt
	r.heard = r.heard || heard
	r.micOn = r.micOn || micOn
	m.mu.Unlock()
	m.changed()
}

// StopRecording ends the current recording (by "user", "auto" or "shutdown"). A user's stop
// holds until the program leaves the microphone: the same meeting is not picked up again.
func (m *Manager) StopRecording(by string) bool {
	m.mu.Lock()
	r := m.rec
	if r == nil {
		m.mu.Unlock()
		return false
	}
	if by == "user" {
		m.det.Decline()
	}
	if r.by == "" {
		r.by = by
		close(r.stop)
	}
	m.mu.Unlock()
	<-r.stopped
	return true
}

func (m *Manager) finishRecording(r *recording, errMsg string) {
	now := m.cfg.Now()
	m.mu.Lock()
	heard, micOn, by := r.heard, r.micOn, cmp.Or(r.by, "error")
	if m.rec == r {
		m.rec = nil
	}
	shutdown := by == "shutdown"
	m.mu.Unlock()
	mt, err := m.store.Update(r.m.ID, func(mt *Meeting) {
		if shutdown {
			// Left in StateRecording: the next run resumes or processes it.
			return
		}
		mt.Ended, mt.StopBy = now, by
		mt.Silent, mt.MicSilent = !heard, !micOn
		mt.State, mt.Error = StateProcessing, errMsg
		if mt.Segments == 0 {
			mt.State = StateFailed
			mt.Error = cmp.Or(errMsg, "没有录到任何音频")
		}
	})
	if err != nil {
		slog.Error("meeting: finish", "meeting", r.m.ID, "err", err)
	}
	if !shutdown {
		m.journal(map[string]any{"meeting": mt.ID, "step": "record_stop", "summary": "录音结束：" + mt.Title,
			"by": by, "seconds": math.Round(mt.Seconds), "error": errMsg})
		if mt.State == StateProcessing {
			m.enqueue(mt.ID)
		}
	}
	m.changed()
}

func (m *Manager) enqueue(id string) {
	select {
	case m.queue <- id:
	default:
		slog.Warn("meeting: processing queue full", "meeting", id)
	}
}

// Retry processes a failed meeting again.
func (m *Manager) Retry(id string) error {
	mt, err := m.store.Update(id, func(mt *Meeting) {
		if mt.State == StateFailed && !mt.AudioDeleted && mt.Segments > 0 {
			mt.State, mt.Error = StateProcessing, ""
		}
	})
	if err != nil {
		return err
	}
	if mt.State != StateProcessing {
		return errors.New("只有失败且音频还在的会议可以重试")
	}
	m.enqueue(id)
	return nil
}

// Retranscribe recognises a finished meeting again in language (one of Languages) and writes a
// new summary; the audio must still be there.
func (m *Manager) Retranscribe(ctx context.Context, id, language string) error {
	if !slices.Contains(Languages, language) {
		return errors.New("不支持的识别语言")
	}
	mt, err := m.store.Get(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	busy := (m.rec != nil && m.rec.m.ID == id) || m.processing == id
	m.mu.Unlock()
	if busy || mt.State == StateRecording || mt.State == StateProcessing {
		return errors.New("会议正在录音或处理中")
	}
	if mt.AudioDeleted {
		return errors.New("音频已删除")
	}
	adir := filepath.Join(m.store.Dir(id), "audio")
	for _, track := range []string{"mic", "remote"} {
		wav, m4a := filepath.Join(adir, track+".wav"), filepath.Join(adir, track+".m4a")
		if _, err := os.Stat(wav); err == nil || len(m.store.segments(id, track)) > 0 {
			continue
		}
		if _, err := os.Stat(m4a); err != nil {
			return errors.New("没有可识别的音频")
		}
		if err := m.cfg.Helper.Encode(ctx, m4a, wav); err != nil {
			return err
		}
	}
	os.Remove(filepath.Join(m.store.Dir(id), "transcript.json"))
	if _, err := m.store.Update(id, func(x *Meeting) {
		x.Language, x.State, x.Stage, x.Error = language, StateProcessing, "", ""
	}); err != nil {
		return err
	}
	m.journal(map[string]any{"meeting": id, "step": "retranscribe", "summary": "重新识别：" + mt.Title, "language": language})
	m.enqueue(id)
	m.changed()
	return nil
}

// housekeeping processes orphans nobody resumed and deletes expired audio.
func (m *Manager) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		m.sweepOrphans()
		m.sweepAudio()
		m.mu.Lock()
		stale := m.cfg.Now().Sub(m.authAt) > authEvery
		m.mu.Unlock()
		if stale {
			m.refreshAuth(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) sweepOrphans() {
	now := m.cfg.Now()
	m.mu.Lock()
	var due []Meeting
	keep := m.orphans[:0]
	for _, o := range m.orphans {
		if now.Sub(lastActive(o)) >= orphanAge {
			due = append(due, o)
		} else {
			keep = append(keep, o)
		}
	}
	m.orphans = keep
	m.mu.Unlock()
	for _, o := range due {
		mt, err := m.store.Update(o.ID, func(mt *Meeting) {
			mt.Ended, mt.StopBy = lastActive(o), "restart"
			mt.State = StateProcessing
			if mt.Segments == 0 {
				mt.State, mt.Error = StateFailed, "没有录到任何音频"
			}
		})
		if err == nil && mt.State == StateProcessing {
			m.enqueue(mt.ID)
		}
	}
}

func (m *Manager) sweepAudio() {
	now := m.cfg.Now()
	for _, mt := range m.store.List() {
		if mt.KeepAudio || mt.AudioDeleted || mt.AudioExpires.IsZero() || now.Before(mt.AudioExpires) {
			continue
		}
		if err := m.DeleteAudio(mt.ID); err != nil {
			slog.Warn("meeting: delete expired audio", "meeting", mt.ID, "err", err)
		}
	}
}

// DeleteAudio removes a meeting's audio now; transcript and summary stay.
func (m *Manager) DeleteAudio(id string) error {
	if !ValidID(id) {
		return os.ErrNotExist
	}
	m.mu.Lock()
	busy := (m.rec != nil && m.rec.m.ID == id) || m.processing == id
	m.mu.Unlock()
	if busy {
		return errors.New("会议正在录音或处理中")
	}
	if err := os.RemoveAll(filepath.Join(m.store.Dir(id), "audio")); err != nil {
		return err
	}
	_, err := m.store.Update(id, func(mt *Meeting) { mt.AudioDeleted = true })
	if err == nil {
		m.journal(map[string]any{"meeting": id, "step": "audio_deleted", "summary": "删除了会议音频"})
	}
	return err
}

// KeepAudio sets whether the audio outlives the 30 days.
func (m *Manager) KeepAudio(id string, keep bool) (Meeting, error) {
	if !ValidID(id) {
		return Meeting{}, os.ErrNotExist
	}
	return m.store.Update(id, func(mt *Meeting) { mt.KeepAudio = keep })
}

func (m *Manager) processLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-m.queue:
			m.mu.Lock()
			m.processing = id
			m.mu.Unlock()
			m.changed()
			err := m.process(ctx, id)
			m.mu.Lock()
			m.processing = ""
			m.mu.Unlock()
			if err != nil && ctx.Err() == nil {
				slog.Warn("meeting: processing failed", "meeting", id, "err", err)
				mt, _ := m.store.Update(id, func(mt *Meeting) {
					mt.State, mt.Error = StateFailed, err.Error()
					if mt.AudioExpires.IsZero() {
						mt.AudioExpires = m.cfg.Now().Add(AudioRetention)
					}
				})
				m.journal(map[string]any{"meeting": id, "step": "failed", "summary": "会议纪要没有做成：" + err.Error()})
				if m.cfg.Notify != nil {
					m.cfg.Notify("meeting:"+id, "会议「"+mt.Title+"」的纪要没有做成", err.Error())
				}
			}
			m.changed()
		}
	}
}

func (m *Manager) stage(id, stage string) {
	m.store.Update(id, func(mt *Meeting) { mt.Stage = stage })
	m.changed()
}

// process turns a recorded meeting into transcript.json / .md, summary.md, promises and a push.
// Each step's output is kept, so a retry after a failure starts where it stopped.
func (m *Manager) process(ctx context.Context, id string) error {
	mt, err := m.store.Get(id)
	if err != nil {
		return err
	}
	dir := m.store.Dir(id)
	lines, err := m.transcribe(ctx, mt)
	if err != nil {
		return err
	}
	mt, _ = m.store.Get(id)
	if m.cfg.Summarize == nil {
		return errors.New("没有纪要会话")
	}
	m.stage(id, "summarizing")
	md, todos, err := m.summarize(ctx, mt, lines)
	if err != nil {
		return fmt.Errorf("纪要：%w", err)
	}
	if err := writeFile(filepath.Join(dir, "summary.md"), []byte(md+"\n")); err != nil {
		return err
	}
	now := m.cfg.Now()
	// A re-recognised meeting keeps the promises its earlier summary made.
	made := map[string]string{}
	for _, t := range mt.Todos {
		if t.Promise != "" {
			made[t.Text] = t.Promise
		}
	}
	for i := range todos {
		t := &todos[i]
		if !t.Mine || m.cfg.Promise == nil {
			continue
		}
		if pid, ok := made[t.Text]; ok {
			t.Promise = pid
			continue
		}
		pid, err := m.cfg.Promise(fmt.Sprintf("%s（会议「%s」）", t.Text, mt.Title), dueOf(t.Due, now))
		if err != nil {
			slog.Warn("meeting: promise", "meeting", id, "err", err)
			continue
		}
		t.Promise = pid
	}
	m.compressAudio(ctx, mt)
	mt, err = m.store.Update(id, func(mt *Meeting) {
		mt.Todos, mt.State, mt.Stage, mt.Error = todos, StateDone, "", ""
		mt.AudioExpires = now.Add(AudioRetention)
	})
	if err != nil {
		return err
	}
	mine := 0
	for _, t := range todos {
		if t.Mine {
			mine++
		}
	}
	m.journal(map[string]any{"meeting": id, "step": "summary", "summary": "会议纪要：" + mt.Title, "todos": len(todos), "mine": mine})
	if m.cfg.Notify != nil {
		body := fmt.Sprintf("时长 %s，%d 条待办", Clock(mt.Seconds), len(todos))
		if mine > 0 {
			body += fmt.Sprintf("（你的 %d 条已放进任务）", mine)
		}
		if mt.Silent {
			body += "。对方那一轨全是静音：检查「屏幕与系统录音」授权"
		}
		m.cfg.Notify("meeting:"+id, "会议「"+mt.Title+"」的纪要好了", body)
	}
	return nil
}

func (m *Manager) transcribe(ctx context.Context, mt Meeting) ([]Line, error) {
	dir := m.store.Dir(mt.ID)
	tpath := filepath.Join(dir, "transcript.json")
	if b, err := os.ReadFile(tpath); err == nil {
		var t struct {
			Lines []Line `json:"lines"`
		}
		if json.Unmarshal(b, &t) == nil {
			return t.Lines, nil
		}
	}
	if m.cfg.Recognizer == nil {
		return nil, errors.New("没有识别引擎")
	}
	if mt.AudioDeleted {
		return nil, errors.New("音频已删除")
	}
	m.stage(mt.ID, "transcribing")
	mic, remote, err := m.joinTracks(mt.ID)
	if err != nil {
		return nil, fmt.Errorf("拼接音频：%w", err)
	}
	hints := Hints{Title: mt.Title, Attendees: mt.Attendees, Language: mt.Language}
	mine, err := m.cfg.Recognizer.Transcribe(ctx, mic, hints)
	if err != nil {
		return nil, fmt.Errorf("识别（你）：%w", err)
	}
	var theirs []Segment
	var turns []Turn
	if !mt.Silent {
		if theirs, err = m.cfg.Recognizer.Transcribe(ctx, remote, hints); err != nil {
			return nil, fmt.Errorf("识别（对方）：%w", err)
		}
		if m.cfg.Diarizer != nil && len(theirs) > 0 {
			m.stage(mt.ID, "diarizing")
			if turns, err = m.cfg.Diarizer.Diarize(ctx, remote); err != nil {
				// Speakers are a refinement: the far end stays one "对方".
				slog.Warn("meeting: diarize", "meeting", mt.ID, "err", err)
				turns = nil
			}
		}
	}
	lines, speakers := Merge(mine, theirs, turns)
	b, _ := json.MarshalIndent(map[string]any{"engine": m.cfg.Recognizer.Name(), "lines": lines}, "", "  ")
	if err := writeFile(tpath, b); err != nil {
		return nil, err
	}
	mt, _ = m.store.Update(mt.ID, func(x *Meeting) { x.Speakers, x.Engine = speakers, m.cfg.Recognizer.Name() })
	if err := writeFile(filepath.Join(dir, "transcript.md"), []byte(TranscriptMarkdown(mt, lines))); err != nil {
		return nil, err
	}
	m.journal(map[string]any{"meeting": mt.ID, "step": "transcript", "summary": "文字稿：" + mt.Title, "lines": len(lines), "speakers": speakers, "engine": m.cfg.Recognizer.Name()})
	return lines, nil
}

// joinTracks writes audio/mic.wav and audio/remote.wav. A segment one track lacks (the helper
// died between the two closes) becomes silence the length of the other, so the tracks stay
// aligned.
func (m *Manager) joinTracks(id string) (string, string, error) {
	adir := filepath.Join(m.store.Dir(id), "audio")
	mic, remote := filepath.Join(adir, "mic.wav"), filepath.Join(adir, "remote.wav")
	idx := map[string]map[string]string{"mic": {}, "remote": {}}
	var names []string
	seen := map[string]bool{}
	for _, track := range []string{"mic", "remote"} {
		for _, p := range m.store.segments(id, track) {
			b := filepath.Base(p)
			idx[track][b] = p
			if !seen[b] {
				seen[b] = true
				names = append(names, b)
			}
		}
	}
	if len(names) == 0 {
		// Decoded back from the AAC copies for a re-recognition.
		if _, err := os.Stat(mic); err == nil {
			if _, err := os.Stat(remote); err == nil {
				return mic, remote, nil
			}
		}
		return "", "", errors.New("没有音频片段")
	}
	slices.Sort(names)
	for _, track := range []string{"mic", "remote"} {
		other := map[string]string{"mic": "remote", "remote": "mic"}[track]
		var srcs []string
		var pads []float64
		for _, n := range names {
			p, ok := idx[track][n]
			if !ok {
				p = filepath.Join(adir, track, n) // missing: padded below
			}
			srcs = append(srcs, p)
			pad, _ := WAVSeconds(idx[other][n])
			pads = append(pads, pad)
		}
		dst := map[string]string{"mic": mic, "remote": remote}[track]
		if _, err := ConcatWAV(dst, srcs, pads); err != nil {
			return "", "", err
		}
	}
	return mic, remote, nil
}

func (m *Manager) summarize(ctx context.Context, mt Meeting, lines []Line) (string, []Todo, error) {
	if len(lines) == 0 {
		return fmt.Sprintf("# %s\n\n没有识别出语音。", mt.Title), nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, summaryTimeout)
	defer cancel()
	reply, err := m.cfg.Summarize(ctx, SummaryKeyPrefix+mt.ID, m.store.Dir(mt.ID), summaryPrompt(mt, transcriptText(lines), m.cfg.Now()))
	if err != nil {
		return "", nil, err
	}
	md, todos := parseSummary(reply)
	if !strings.HasPrefix(md, "# ") {
		md = "# " + mt.Title + " · 纪要\n\n" + md
	}
	return md, todos, nil
}

// compressAudio replaces the WAV segments with one AAC file per track (about a seventh of the
// size) for the 30 days the audio is kept. On failure the WAVs stay.
func (m *Manager) compressAudio(ctx context.Context, mt Meeting) {
	adir := filepath.Join(m.store.Dir(mt.ID), "audio")
	for _, track := range []string{"mic", "remote"} {
		wav := filepath.Join(adir, track+".wav")
		if _, err := os.Stat(wav); err != nil {
			continue
		}
		if err := m.cfg.Helper.Encode(ctx, wav, filepath.Join(adir, track+".m4a")); err != nil {
			slog.Warn("meeting: encode", "meeting", mt.ID, "track", track, "err", err)
			return
		}
	}
	for _, track := range []string{"mic", "remote"} {
		os.RemoveAll(filepath.Join(adir, track))
		os.Remove(filepath.Join(adir, track+".wav"))
	}
}

// Stop waits for the background work Run started; Run's ctx must be cancelled first.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.StopRecording("shutdown")
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}
