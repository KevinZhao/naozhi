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
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/session/spawnpool"
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

// errInitAuth is a spawn whose CLI failed the Init handshake, as Spawn
// returns it.
var errInitAuth = fmt.Errorf("%w: %w", clierr.ErrSpawnInit, &clierr.ProcessExitedError{Code: 1, Class: clierr.ExitAuth})

// spawnRun reads key's run of failed spawns.
func spawnRun(r *Router, key string) (f spawnpool.StartupFailure, ok bool) {
	r.ss.Update(func(tx sessTx) { f, ok = tx.Ext().spawns.StartupFailure(key) })
	return f, ok
}

// ageSpawnRun moves key's run of failed spawns d into the past.
func ageSpawnRun(r *Router, key string, d time.Duration) {
	r.ss.Update(func(tx sessTx) {
		f, _ := tx.Ext().spawns.StartupFailure(key)
		f.At = f.At.Add(-d)
		tx.Ext().spawns.NoteStartupFailure(key, f)
	})
}

// newSpawnFailRouter is a router whose spawns fail the Init handshake with
// *spawnErr until it is set to nil; spawns are counted.
func newSpawnFailRouter(t *testing.T) (r *Router, spawnErr *error, spawns *int) {
	t.Helper()
	r = newResumeGuardRouter(t)
	spawnErr, spawns = new(error), new(int)
	*spawnErr = errInitAuth
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
		*spawns++
		if *spawnErr != nil {
			return nil, *spawnErr
		}
		return newIdleProc(), nil
	}
	return r, spawnErr, spawns
}

// A key whose spawns keep failing the Init handshake pauses like one whose
// CLI dies at startup, with no session in the table to carry the streak; the
// session that finally starts inherits it.
func TestGetOrCreate_SpawnInitFailuresPauseANewKey(t *testing.T) {
	t.Parallel()
	r, spawnErr, spawns := newSpawnFailRouter(t)
	get := func() error {
		_, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
		return err
	}
	for i := 1; i <= 2; i++ {
		if err := get(); !errors.Is(err, clierr.ErrSpawnInit) || *spawns != i {
			t.Fatalf("failure %d: err = %v after %d spawns; want the spawn's error", i, err, *spawns)
		}
	}
	if err := get(); !errors.Is(err, ErrCLIStartupFailed) || *spawns != 2 {
		t.Fatalf("after two failures: err = %v after %d spawns; want ErrCLIStartupFailed without a spawn", err, *spawns)
	}
	ageSpawnRun(r, sfKey, 31*time.Second)
	if err := get(); !errors.Is(err, clierr.ErrSpawnInit) || *spawns != 3 {
		t.Fatalf("after the cooldown: err = %v after %d spawns; want one more spawn", err, *spawns)
	}
	ageSpawnRun(r, sfKey, 31*time.Second)
	if err := get(); !errors.Is(err, ErrCLIStartupFailed) || !strings.Contains(err.Error(), "3 in a row") || *spawns != 3 {
		t.Fatalf("third failure 31s ago: err = %v after %d spawns; want a longer pause", err, *spawns)
	}
	ageSpawnRun(r, sfKey, 30*time.Second)
	*spawnErr = nil
	s, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
	if err != nil || *spawns != 4 {
		t.Fatalf("after the longer cooldown: err = %v after %d spawns; want a session", err, *spawns)
	}
	if got := s.startupFails.Load(); got != 3 {
		t.Errorf("session startupFails = %d, want the run's 3", got)
	}
	if f, ok := spawnRun(r, sfKey); ok {
		t.Errorf("run after a started session = %+v, want none", f)
	}
}

// A failed spawn continues the streak of the dead entry's process, so a CLI
// alternating between dying at startup and failing Init keeps backing off.
func TestGetOrCreate_SpawnInitFailureContinuesTheEntrysStreak(t *testing.T) {
	t.Parallel()
	r, dead, _ := newStartupFailRouter(t, newStartupFailedProc(clierr.ExitAuth, time.Now().Add(-31*time.Second)))
	dead.startupFails.Store(1)
	spawns := 0
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
		spawns++
		return nil, errInitAuth
	}
	if _, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{}); !errors.Is(err, clierr.ErrSpawnInit) {
		t.Fatalf("GetOrCreate err = %v, want the spawn's error", err)
	}
	_, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
	if !errors.Is(err, ErrCLIStartupFailed) || !strings.Contains(err.Error(), "3 in a row") || spawns != 1 {
		t.Fatalf("GetOrCreate err = %v after %d spawns; want a pause 3 in a row after one spawn", err, spawns)
	}
	if cur := r.ss.Load(sfKey); cur != dead {
		t.Errorf("the key holds %p, want the dead entry %p kept", cur, dead)
	}
}

// /new lifts the pause at once, on a key with no session too.
func TestGetOrCreate_SpawnInitPauseLiftedByReset(t *testing.T) {
	t.Parallel()
	get := func(r *Router) (*ManagedSession, error) {
		s, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
		return s, err
	}
	// Each reset returns the session the next message gets.
	resets := map[string]func(r *Router) (*ManagedSession, error){
		"Reset":                   func(r *Router) (*ManagedSession, error) { r.Reset(sfKey); return get(r) },
		"ResetAndDiscardOverride": func(r *Router) (*ManagedSession, error) { r.ResetAndDiscardOverride(sfKey); return get(r) },
		"ResetAndRecreate": func(r *Router) (*ManagedSession, error) {
			return r.ResetAndRecreate(context.Background(), sfKey, AgentOpts{})
		},
	}
	for name, reset := range resets {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, spawnErr, spawns := newSpawnFailRouter(t)
			for range 2 {
				_, _ = get(r)
			}
			if _, ok := spawnRun(r, sfKey); !ok || r.ss.Load(sfKey) != nil {
				t.Fatal("setup: want a run of failed spawns and no session")
			}
			*spawnErr = nil
			s, err := reset(r)
			if err != nil || *spawns != 3 || s.startupFails.Load() != 0 {
				t.Fatalf("after %s: err = %v after %d spawns; want a third spawn and a session with no streak", name, err, *spawns)
			}
		})
	}
}

// Removing a dead entry for good, from the dashboard or by /cd, lifts the
// pause of the key's run along with it.
func TestGetOrCreate_SpawnInitPauseLiftedByRemoval(t *testing.T) {
	t.Parallel()
	removals := map[string]func(r *Router){
		"Remove":                   func(r *Router) { r.Remove(sfKey) },
		"ResetChatAndSetWorkspace": func(r *Router) { r.ResetChatAndSetWorkspace("feishu:direct:alice", t.TempDir()) },
	}
	for name, remove := range removals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, spawnErr, spawns := newSpawnFailRouter(t)
			injectSession(r, sfKey, newDeadProc())
			for range 2 {
				_, _, _ = r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
			}
			if _, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{}); !errors.Is(err, ErrCLIStartupFailed) || *spawns != 2 {
				t.Fatalf("setup: err = %v after %d spawns; want a paused dead entry", err, *spawns)
			}
			remove(r)
			if f, ok := spawnRun(r, sfKey); ok {
				t.Errorf("run after %s = %+v, want none", name, f)
			}
			*spawnErr = nil
			if _, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{}); err != nil || *spawns != 3 {
				t.Errorf("after %s: err = %v after %d spawns; want a third spawn", name, err, *spawns)
			}
		})
	}
}

// A live session renamed onto a key ends that key's run, so its own death
// later respawns instead of pausing on failures that were never its own.
func TestRenameSession_LiveSessionEndsTheTargetsRun(t *testing.T) {
	t.Parallel()
	const from = "feishu:direct:alice:scratch"
	r, spawnErr, spawns := newSpawnFailRouter(t)
	for range 2 {
		_, _, _ = r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
	}
	proc := newIdleProc()
	injectSession(r, from, proc)
	if !r.RenameSession(from, sfKey) {
		t.Fatal("RenameSession refused")
	}
	if f, ok := spawnRun(r, sfKey); ok {
		t.Errorf("run after the rename = %+v, want none", f)
	}
	proc.mu.Lock()
	proc.isAlive = false
	proc.mu.Unlock()
	*spawnErr = nil
	if _, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{}); err != nil || *spawns != 3 {
		t.Errorf("after the renamed session died: err = %v after %d spawns; want a respawn", err, *spawns)
	}
}

// Only a failed Init handshake on a live call counts: a rejected resume is
// retried fresh, and that retry's failure is the one counted; an abandoned
// call, a key another path brought up meanwhile, or a failure before the CLI
// runs says nothing about the CLI.
func TestGetOrCreate_SpawnFailuresThatDoNotCount(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // the rejected resume's retry waits on the key's socket
	t.Run("rejected resume", func(t *testing.T) {
		r, _, resumes := newStartupFailRouter(t, newDeadProc())
		rejected := fmt.Errorf("%w: %w", clierr.ErrSpawnInit, clierr.ErrResumeRejected)
		r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
			*resumes = append(*resumes, opts.ResumeID)
			if opts.ResumeID != "" {
				return nil, rejected
			}
			return nil, errInitAuth
		}
		_, _, _ = r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
		if f, _ := spawnRun(r, sfKey); f.Streak != 1 || !slices.Equal(*resumes, []string{sfSID, ""}) {
			t.Errorf("run streak = %d after spawns %q; want 1, for the fresh retry only", f.Streak, *resumes)
		}
	})
	t.Run("cancelled call", func(t *testing.T) {
		r, _, _ := newSpawnFailRouter(t)
		ctx, cancel := context.WithCancel(context.Background())
		r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
			cancel()
			return nil, errInitAuth
		}
		_, _, _ = r.GetOrCreate(ctx, sfKey, AgentOpts{})
		if f, ok := spawnRun(r, sfKey); ok {
			t.Errorf("run = %+v, want none", f)
		}
	})
	t.Run("a live session installed meanwhile", func(t *testing.T) {
		r, _, _ := newSpawnFailRouter(t)
		r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
			injectSession(r, sfKey, newIdleProc())
			return nil, errInitAuth
		}
		_, _, _ = r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
		if f, ok := spawnRun(r, sfKey); ok {
			t.Errorf("run = %+v, want none", f)
		}
	})
	t.Run("shim start failure", func(t *testing.T) {
		r, spawnErr, _ := newSpawnFailRouter(t)
		*spawnErr = errors.New("start shim: shim already listening: refusing to clobber")
		_, _, _ = r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
		if f, ok := spawnRun(r, sfKey); ok {
			t.Errorf("run = %+v, want none", f)
		}
	})
}

// Callers parked on a failing spawn wake to the pause it recorded instead of
// each spawning again.
func TestGetOrCreate_SpawnInitFailurePausesItsWaiters(t *testing.T) {
	t.Parallel()
	r := newResumeGuardRouter(t)
	r.ss.Update(func(tx sessTx) {
		tx.Ext().spawns.NoteStartupFailure(sfKey, spawnpool.StartupFailure{Streak: 1, At: time.Now()})
	})
	started, release := make(chan struct{}), make(chan struct{})
	var spawns atomic.Int32
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
		if spawns.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil, errInitAuth
	}
	const n = 8
	errs := make(chan error, n)
	get := func() {
		_, _, err := r.GetOrCreate(context.Background(), sfKey, AgentOpts{})
		errs <- err
	}
	go get()
	<-started
	for range n - 1 {
		go get()
	}
	close(release)
	paused := 0
	for range n {
		if errors.Is(<-errs, ErrCLIStartupFailed) {
			paused++
		}
	}
	if spawns.Load() != 1 || paused != n-1 {
		t.Errorf("%d spawns, %d of %d callers paused; want one spawn and every other caller paused", spawns.Load(), paused, n)
	}
}

// Cleanup drops the run of a key that was not retried for two maximum
// cooldowns.
func TestCleanup_PrunesStaleSpawnRuns(t *testing.T) {
	t.Parallel()
	r := newResumeGuardRouter(t)
	r.ss.Update(func(tx sessTx) {
		tx.Ext().spawns.NoteStartupFailure("stale", spawnpool.StartupFailure{Streak: 9, At: time.Now().Add(-2*startupCooldownMax - time.Minute)})
		tx.Ext().spawns.NoteStartupFailure("recent", spawnpool.StartupFailure{Streak: 9, At: time.Now().Add(-startupCooldownMax)})
	})
	r.Cleanup()
	if _, ok := spawnRun(r, "stale"); ok {
		t.Error("stale run survived Cleanup")
	}
	if _, ok := spawnRun(r, "recent"); !ok {
		t.Error("Cleanup pruned a run still within two maximum cooldowns")
	}
}
