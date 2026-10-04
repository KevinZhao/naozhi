package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/shim"
)

// startupFailedProc is a dead process whose CLI failed at startup.
type startupFailedProc struct {
	*fakeProcess
	class clierr.ExitClass
	at    time.Time
}

func (p *startupFailedProc) StartupFailure() (clierr.ExitClass, time.Time, bool) {
	return p.class, p.at, true
}

func newStartupFailedProc(class clierr.ExitClass, at time.Time) *startupFailedProc {
	return &startupFailedProc{fakeProcess: newDeadProc(), class: class, at: at}
}

const (
	sfKey = "feishu:direct:alice:general"
	sfWS  = "/home/u/proj"
	sfSID = "sess-1"
)

// newStartupFailRouter is a router whose resume guard finds sfSID's
// transcript, with a dead session for sfKey running proc; spawns are counted
// and their resume ids kept.
func newStartupFailRouter(t *testing.T, proc processIface) (*Router, *ManagedSession, *[]string) {
	t.Helper()
	r := newResumeGuardRouter(t)
	jsonl := claudefs.SessionJSONL(r.hist.claudeDir, sfWS, sfSID)
	if err := os.MkdirAll(filepath.Dir(jsonl), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonl, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resumes := &[]string{}
	r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
		*resumes = append(*resumes, opts.ResumeID)
		return newIdleProc(), nil
	}
	dead := injectSession(r, sfKey, proc)
	dead.setWorkspace(sfWS)
	dead.setSessionID(sfSID)
	return r, dead, resumes
}

// A CLI that failed at startup on a resume id the failure may be about is
// respawned fresh, keeping its workspace and chaining the old id; a cause
// that fails fresh spawns alike keeps the resume. The replacement inherits the
// streak either way.
func TestGetOrCreate_StartupFailureDropsResume(t *testing.T) {
	t.Parallel()
	failed := func(c clierr.ExitClass) processIface { return newStartupFailedProc(c, time.Now()) }
	cases := []struct {
		name       string
		proc       processIface
		wantResume string
		wantStatus SessionStatus
		wantFails  int32
	}{
		{"stale resume id", failed(clierr.ExitResumeNotFound), "", SessionResumeLost, 1},
		{"unnamed cause", failed(clierr.ExitUnknown), "", SessionResumeLost, 1},
		{"auth", failed(clierr.ExitAuth), sfSID, SessionResumed, 1},
		{"mcp config", failed(clierr.ExitMCPConfig), sfSID, SessionResumed, 1},
		{"missing runtime", failed(clierr.ExitMissingRuntime), sfSID, SessionResumed, 1},
		{"died after startup", newDeadProc(), sfSID, SessionResumed, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _, resumes := newStartupFailRouter(t, tc.proc)
			s, st, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(*resumes, []string{tc.wantResume}) || st != tc.wantStatus {
				t.Errorf("spawn resumes = %q, status = %d; want [%q], %d", *resumes, st, tc.wantResume, tc.wantStatus)
			}
			if got := s.startupFails.Load(); got != tc.wantFails {
				t.Errorf("replacement startupFails = %d, want %d", got, tc.wantFails)
			}
			if s.Workspace() != sfWS {
				t.Errorf("replacement workspace = %q, want the old session's %q", s.Workspace(), sfWS)
			}
			if tc.wantResume == "" && !slices.Contains(s.prevSessionIDs, sfSID) {
				t.Errorf("prevSessionIDs = %q, want the dropped %q chained", s.prevSessionIDs, sfSID)
			}
		})
	}
}

// From the second startup failure in a row the key waits out a cooldown
// before it respawns, without spawning; /new spawns at once and clears the
// streak.
func TestGetOrCreate_StartupBreaker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		before    int32 // failures in a row before the dead process's own
		ago       time.Duration
		wantPause bool
	}{
		{"first failure respawns", 0, 0, false},
		{"second failure pauses", 1, 0, true},
		{"second failure after its cooldown", 1, 31 * time.Second, false},
		{"third failure waits longer", 2, 31 * time.Second, true},
		{"third failure after its cooldown", 2, 61 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, dead, resumes := newStartupFailRouter(t, newStartupFailedProc(clierr.ExitAuth, time.Now().Add(-tc.ago)))
			dead.startupFails.Store(tc.before)
			s, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
			if tc.wantPause {
				if !errors.Is(err, ErrCLIStartupFailed) || len(*resumes) != 0 {
					t.Fatalf("GetOrCreate err = %v after %d spawns; want ErrCLIStartupFailed and none", err, len(*resumes))
				}
				if cur := r.ss.Load(sfKey); cur != dead || stateOf(r).spawns.SpawningCount() != 0 {
					t.Errorf("the paused key holds %p (want the dead %p), %d spawns in flight", cur, dead, stateOf(r).spawns.SpawningCount())
				}
				return
			}
			if err != nil || len(*resumes) != 1 {
				t.Fatalf("GetOrCreate err = %v after %d spawns; want one spawn", err, len(*resumes))
			}
			if got := s.startupFails.Load(); got != tc.before+1 {
				t.Errorf("replacement startupFails = %d, want %d", got, tc.before+1)
			}
		})
	}
	t.Run("/new during the pause", func(t *testing.T) {
		t.Parallel()
		r, dead, resumes := newStartupFailRouter(t, newStartupFailedProc(clierr.ExitAuth, time.Now()))
		dead.startupFails.Store(4)
		s, err := r.ResetAndRecreate(context.Background(), sfKey, AgentOpts{})
		if err != nil || !slices.Equal(*resumes, []string{""}) {
			t.Fatalf("ResetAndRecreate err = %v, spawn resumes %q; want one fresh spawn", err, *resumes)
		}
		if got := s.startupFails.Load(); got != 0 {
			t.Errorf("startupFails after /new = %d, want 0", got)
		}
	})
}

func TestStartupFailure_CooldownLeft(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_000_000, 0)
	cases := []struct {
		streak int32
		after  time.Duration
		want   time.Duration
	}{
		{0, 0, 0},
		{1, 0, 0},
		{2, 0, 30 * time.Second},
		{2, 10 * time.Second, 20 * time.Second},
		{2, time.Minute, 0},
		{3, 0, time.Minute},
		{6, 0, 8 * time.Minute},
		{7, 0, 10 * time.Minute},
		{40, 0, 10 * time.Minute},
	}
	for _, tc := range cases {
		f := startupFailure{streak: tc.streak, at: at}
		if got := f.cooldownLeft(at.Add(tc.after)); got != tc.want {
			t.Errorf("streak %d, %s after: cooldownLeft = %s, want %s", tc.streak, tc.after, got, tc.want)
		}
	}
}

// A resume the backend refuses during the spawn handshake is retried fresh in
// the same call, once: the user's message has not been sent yet.
func TestGetOrCreate_RetriesARejectedResumeFresh(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // the retry waits on the key's shim socket
	rejected := fmt.Errorf("protocol init: acp session/load: %w", clierr.ErrResumeRejected)
	t.Run("fresh spawn succeeds", func(t *testing.T) {
		r, _, resumes := newStartupFailRouter(t, newDeadProc())
		r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
			*resumes = append(*resumes, opts.ResumeID)
			if opts.ResumeID != "" {
				return nil, rejected
			}
			return newIdleProc(), nil
		}
		s, st, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
		if err != nil || !slices.Equal(*resumes, []string{sfSID, ""}) || st != SessionResumeLost {
			t.Fatalf("GetOrCreate err = %v, status %d, spawn resumes %q; want a fresh retry reporting SessionResumeLost", err, st, *resumes)
		}
		if !slices.Contains(s.prevSessionIDs, sfSID) {
			t.Errorf("prevSessionIDs = %q, want the rejected %q chained", s.prevSessionIDs, sfSID)
		}
	})
	t.Run("fresh spawn fails too", func(t *testing.T) {
		r, _, resumes := newStartupFailRouter(t, newDeadProc())
		r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
			*resumes = append(*resumes, opts.ResumeID)
			return nil, rejected
		}
		if _, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{}); !errors.Is(err, clierr.ErrResumeRejected) {
			t.Fatalf("GetOrCreate err = %v, want the fresh spawn's error", err)
		}
		if !slices.Equal(*resumes, []string{sfSID, ""}) {
			t.Errorf("spawn resumes = %q, want one resume then one fresh retry", *resumes)
		}
	})
}

// The fresh retry of a rejected resume waits for the failed spawn's shim to
// release the key's socket, as a real StartShim refuses to clobber a bound
// one; a socket that outlives the wait makes a failed retry ErrShimStuck.
func TestGetOrCreate_RejectedResumeRetryWaitsForTheSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: waits out the 2s socket-gone timeout")
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	sock := shim.SocketPath(shim.KeyHash(sfKey))
	rejected := fmt.Errorf("protocol init: acp session/load: %w", clierr.ErrResumeRejected)
	errClobber := errors.New("start shim: shim already listening: refusing to clobber")
	for _, released := range []bool{true, false} {
		if err := os.WriteFile(sock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		r, _, resumes := newStartupFailRouter(t, newDeadProc())
		r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
			*resumes = append(*resumes, opts.ResumeID)
			if opts.ResumeID != "" {
				if released {
					time.AfterFunc(100*time.Millisecond, func() { os.Remove(sock) })
				}
				return nil, rejected
			}
			if _, err := os.Stat(sock); err == nil {
				return nil, errClobber
			}
			return newIdleProc(), nil
		}
		_, st, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
		if !slices.Equal(*resumes, []string{sfSID, ""}) {
			t.Fatalf("released=%v: spawn resumes = %q, want one resume then one fresh retry", released, *resumes)
		}
		if released && (err != nil || st != SessionResumeLost) {
			t.Errorf("socket released: GetOrCreate err = %v, status %d; want the fresh retry to succeed", err, st)
		}
		if !released && (!errors.Is(err, ErrShimStuck) || !errors.Is(err, errClobber)) {
			t.Errorf("socket stays bound: GetOrCreate err = %v; want ErrShimStuck wrapping the retry's error", err)
		}
		os.Remove(sock)
	}
}

// A key the breaker pauses logs the pause, not a resume that never runs.
func TestGetOrCreate_StartupBreakerLogsNoResume(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	r, dead, _ := newStartupFailRouter(t, newStartupFailedProc(clierr.ExitAuth, time.Now()))
	dead.startupFails.Store(1)
	if _, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{}); !errors.Is(err, ErrCLIStartupFailed) {
		t.Fatalf("GetOrCreate err = %v, want ErrCLIStartupFailed", err)
	}
	if out := buf.String(); strings.Contains(out, "resuming") || !strings.Contains(out, "respawn paused") {
		t.Errorf("log = %q; want the pause and no resuming line", out)
	}
}
