package session

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
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
// owner's back), a turn still running, a Send holding sendMu, and a session
// whose process is already gone. Each leaves the process as it was.
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
