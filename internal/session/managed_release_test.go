package session

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// sidProc is an idle fakeProcess that reports a CLI session id, so a spawn
// records it the way a real claude spawn records its first result's id.
type sidProc struct {
	*fakeProcess
	sid string
}

func (p sidProc) SessionID() string { return p.sid }

// resumeRecorder is a spawn hook handing out one sidProc per spawn and
// recording the ResumeID each spawn asked for.
type resumeRecorder struct {
	mu      sync.Mutex
	resumes []string
	procs   []sidProc
}

func (h *resumeRecorder) hook(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resumes = append(h.resumes, opts.ResumeID)
	p := sidProc{fakeProcess: newIdleProc(), sid: "sid-" + strconv.Itoa(len(h.procs)+1)}
	if opts.ResumeID != "" {
		p.sid = opts.ResumeID
	}
	h.procs = append(h.procs, p)
	return p, nil
}

// pendingProc is an idle process holding one passthrough message the CLI has
// not started yet.
type pendingProc struct{ *fakeProcess }

func (pendingProc) PassthroughDepth() int { return 1 }

// liveSendProc fails a Send that reaches it after it was closed, as a real
// process returns ErrProcessExited.
type liveSendProc struct{ *fakeProcess }

func (p liveSendProc) Send(ctx context.Context, text string, images []clievent.Attachment, cb clievent.EventCallback) (*clievent.SendResult, error) {
	if !p.Alive() {
		return nil, clierr.ErrProcessExited
	}
	return p.fakeProcess.Send(ctx, text, images, cb)
}

// heldPassthroughProc parks SendPassthrough until release is closed, the way
// a queued message waits for the CLI to start it.
type heldPassthroughProc struct {
	*fakeProcess
	entered, release chan struct{}
}

func (p heldPassthroughProc) SendPassthrough(ctx context.Context, text string, images []clievent.Attachment, cb clievent.EventCallback, _ string) (*clievent.SendResult, error) {
	close(p.entered)
	<-p.release
	if !p.Alive() {
		return nil, clierr.ErrProcessExited
	}
	return p.fakeProcess.Send(ctx, text, images, cb)
}

func cronAliveCount(r *Router) (n int) {
	r.ss.View(func(v sessView) { n, _ = countExemptCombined(v, "cron") })
	return n
}

// A released cron session keeps its entry and id: the process is closed, the
// cron namespace stops counting it, and the next GetOrCreate resumes the same
// conversation instead of starting a new one.
func TestReleaseIdleProcess_NextGetOrCreateResumesSameSession(t *testing.T) {
	h := &resumeRecorder{}
	r := spawnRouter(t, 4, h.hook)
	const key = "cron:job-release"

	s, st, err := r.GetOrCreate(context.Background(), key, AgentOpts{Exempt: true})
	if err != nil || st != SessionNew {
		t.Fatalf("first GetOrCreate: status=%v err=%v", st, err)
	}
	if got := s.SessionID(); got != "sid-1" {
		t.Fatalf("session id = %q, want sid-1", got)
	}
	if cronAliveCount(r) != 1 {
		t.Fatalf("cron alive count = %d before release, want 1", cronAliveCount(r))
	}

	if !s.ReleaseIdleProcess() {
		t.Fatal("ReleaseIdleProcess refused an idle exempt session")
	}
	if h.procs[0].Alive() {
		t.Error("released process is still alive")
	}
	if got := s.DeathReason(); got != DeathReasonReleased {
		t.Errorf("death reason = %q, want %q", got, DeathReasonReleased)
	}
	if r.SessionFor(key) != s {
		t.Fatal("release dropped the session entry")
	}
	if n := cronAliveCount(r); n != 0 {
		t.Errorf("cron alive count = %d after release, want 0", n)
	}

	s2, st, err := r.GetOrCreate(context.Background(), key, AgentOpts{Exempt: true})
	if err != nil || st != SessionResumed {
		t.Fatalf("second GetOrCreate: status=%v err=%v, want SessionResumed", st, err)
	}
	if got := s2.SessionID(); got != "sid-1" {
		t.Errorf("resumed session id = %q, want sid-1", got)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.resumes) != 2 || h.resumes[1] != "sid-1" {
		t.Errorf("spawn resume ids = %q, want [\"\" \"sid-1\"]", h.resumes)
	}
}

// What ReleaseIdleProcess refuses: a user session (never released behind its
// owner's back), a turn still running, a Send holding or queued on sendMu, a
// passthrough message the CLI has not started, and a session whose process is
// already gone. Each leaves the process as it was.
func TestReleaseIdleProcess_Refusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func() (*ManagedSession, *fakeProcess)
	}{
		{"not_exempt", func() (*ManagedSession, *fakeProcess) {
			p := newIdleProc()
			s := &ManagedSession{key: "feishu:direct:alice:general"}
			s.storeProcess(p)
			return s, p
		}},
		{"running", func() (*ManagedSession, *fakeProcess) {
			p := newRunningProc()
			s := &ManagedSession{key: "cron:job-run", exempt: true}
			s.storeProcess(p)
			return s, p
		}},
		{"send_in_flight", func() (*ManagedSession, *fakeProcess) {
			p := newIdleProc()
			s := &ManagedSession{key: "cron:job-send", exempt: true}
			s.storeProcess(p)
			s.sendMu.Lock()
			return s, p
		}},
		{"turn_waiting", func() (*ManagedSession, *fakeProcess) {
			p := newIdleProc()
			s := &ManagedSession{key: "cron:job-wait", exempt: true}
			s.storeProcess(p)
			s.turnWaiters.Add(1)
			return s, p
		}},
		{"passthrough_pending", func() (*ManagedSession, *fakeProcess) {
			p := newIdleProc()
			s := &ManagedSession{key: "cron:job-pt", exempt: true}
			s.storeProcess(pendingProc{p})
			return s, p
		}},
		{"dead", func() (*ManagedSession, *fakeProcess) {
			p := newDeadProc()
			s := &ManagedSession{key: "cron:job-dead", exempt: true}
			s.storeProcess(p)
			return s, p
		}},
		{"no_process", func() (*ManagedSession, *fakeProcess) {
			return &ManagedSession{key: "cron:job-none", exempt: true}, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, p := tc.setup()
			wasAlive := p != nil && p.Alive()
			if s.ReleaseIdleProcess() {
				t.Fatal("ReleaseIdleProcess = true, want a refusal")
			}
			if p != nil && p.Alive() != wasAlive {
				t.Error("a refused release changed the process")
			}
			if got := s.DeathReason(); got != "" {
				t.Errorf("a refused release stamped death reason %q", got)
			}
		})
	}
}

// A dashboard message queued on sendMu behind the cron run's Send must not
// lose its process: the release that runs as the cron Send unlocks would
// otherwise win the lock (TryLock barges past a woken waiter) and close it.
func TestReleaseIdleProcess_RefusedWhileSendQueued(t *testing.T) {
	t.Parallel()
	p := liveSendProc{newIdleProc()}
	s := &ManagedSession{key: "cron:job-queued", exempt: true}
	s.storeProcess(p)

	s.sendMu.Lock() // the cron run's Send
	errc := make(chan error, 1)
	go func() {
		_, err := s.Send(context.Background(), "follow-up", nil, nil)
		errc <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for s.turnWaiters.Load() == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	s.sendMu.Unlock()
	if s.ReleaseIdleProcess() {
		t.Error("ReleaseIdleProcess closed the process a queued Send was waiting for")
	}
	if err := <-errc; err != nil {
		t.Errorf("queued Send: %v, want it to reach a live process", err)
	}
	if !p.Alive() {
		t.Error("process closed")
	}
	if !s.ReleaseIdleProcess() {
		t.Error("release refused once the queued Send returned")
	}
}

// A passthrough message handed to the CLI but not yet started leaves the
// process idle and sendMu free; the release must still wait for it.
func TestReleaseIdleProcess_RefusedDuringPassthroughSend(t *testing.T) {
	t.Parallel()
	p := heldPassthroughProc{fakeProcess: newIdleProc(), entered: make(chan struct{}), release: make(chan struct{})}
	s := &ManagedSession{key: "cron:job-passthrough", exempt: true}
	s.storeProcess(p)

	errc := make(chan error, 1)
	go func() {
		_, err := s.SendPassthrough(context.Background(), "follow-up", nil, nil, "")
		errc <- err
	}()
	<-p.entered
	released := s.ReleaseIdleProcess()
	close(p.release)
	if err := <-errc; err != nil {
		t.Errorf("SendPassthrough: %v", err)
	}
	if released {
		t.Error("ReleaseIdleProcess closed the process under a pending passthrough message")
	}
}

// Releasing after each run is what lets more persistent jobs exist than the
// cron namespace admits alive at once: thirteen jobs run one after another
// under a cap of twelve, and the thirteenth would be refused without it.
func TestReleaseIdleProcess_SequentialJobsBeyondCronCap(t *testing.T) {
	h := &resumeRecorder{}
	r := spawnRouter(t, 4, h.hook)
	for i := range maxCronExempt + 1 {
		key := "cron:job-" + strconv.Itoa(i)
		s, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{Exempt: true})
		if err != nil {
			t.Fatalf("job %d: GetOrCreate: %v (ErrMaxExemptSessions=%v)", i, err, errors.Is(err, ErrMaxExemptSessions))
		}
		if !s.ReleaseIdleProcess() {
			t.Fatalf("job %d: release refused", i)
		}
	}
	if n := cronAliveCount(r); n != 0 {
		t.Errorf("cron alive count = %d after every run released, want 0", n)
	}
}
