package session

// R70-ARCH-H3 regression tests for Router.Takeover.
//
// Takeover has three branches that previously had zero direct coverage:
//  1. Fresh key — no existing session, the spawn runs immediately.
//  2. Replace alive / dead session — existing session is closed and
//     unregistered before the re-spawn.
//  3. Concurrent-creation abort — while we release the table lock to Close() the
//     old process, another goroutine slips in a live session under the
//     same key; Takeover must abort with a specific error instead of
//     clobbering the interloper.
//
// The real spawn call at the end of Takeover fails because newTestRouter's
// wrapper points at /nonexistent/cli-binary. That's fine: these tests
// assert the side effects up to and including the spawn, not a
// successful spawn.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/shim"
)

// newTakeoverTestRouter builds a Router that has every map Takeover and
// the spawn touch which the older newTestRouter helper leaves nil
// (wsStore and pp are zero-value usable and need no init).
func newTakeoverTestRouter(maxProcs int) *Router {
	r := newTestRouter(maxProcs)
	stateOf(r).picks.backend = map[string]string{}
	return r
}

// TestTakeover_NewKey — calling Takeover on a key that has no existing
// session must skip the close/unregister branches entirely, still write
// the workspace override, and then proceed to spawn (which fails in tests
// but only after the override is recorded).
func TestTakeover_NewKey(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user1:general"
	workspace := "/tmp/takeover-ws"

	_, err := r.Takeover(context.Background(), key, "sess-abc", workspace, AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error (nonexistent CLI), got nil")
	}
	if !strings.Contains(err.Error(), "spawn") {
		t.Errorf("error should be a spawn failure, got: %v", err)
	}

	// Workspace override must land on the chat key prefix, not the session key.
	chatKey := chatKeyFor(key)
	if got, _ := stateOf(r).workspaces.Lookup(chatKey); got != workspace {
		t.Errorf("workspaceOverrides[%q] = %q, want %q", chatKey, got, workspace)
	}
	if !stateOf(r).workspaces.Dirty() {
		t.Error("wsOverridesDirty should be set after Takeover writes override")
	}

	// No stale session should have been injected.
	if _, ok := lookupT(r, key); ok {
		t.Error("sessions[key] should be empty after failed spawn on a fresh Takeover")
	}
}

// TestTakeover_ReplacesDeadSession — when the existing session's process
// is dead, Takeover takes the else-branch that unregisters without calling
// Close, and bumps storeGen.
func TestTakeover_ReplacesDeadSession(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user2:general"

	old := injectSession(r, key, newDeadProc())
	old.setSessionID("old-sess")
	setIDT(r, "old-sess", key)
	genBefore := r.ss.Gen()

	_, err := r.Takeover(context.Background(), key, "new-sess", "/tmp/ws", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error after dead-session unregister")
	}

	if _, ok := lookupT(r, key); ok {
		t.Error("dead session should have been unregistered")
	}
	if _, ok := keyForIDT(r, "old-sess"); ok {
		t.Error("old session ID should have been removed from sessionIDToKey")
	}
	if r.ss.Gen() <= genBefore {
		t.Error("storeGen should advance when dead session is unregistered")
	}
}

// TestTakeover_ReplacesAliveSession — when the existing session's process
// is alive, Takeover enters the close-and-recheck branch: Close() is
// called on the old process while the table lock is released, then the session is
// unregistered under the re-acquired lock. the spawn fails afterward,
// but the old process must be Close()'d and the session gone.
func TestTakeover_ReplacesAliveSession(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user3:general"

	oldProc := newIdleProc()
	old := injectSession(r, key, oldProc)
	old.setSessionID("old-alive-sess")
	setIDT(r, "old-alive-sess", key)

	_, err := r.Takeover(context.Background(), key, "new-sess", "/tmp/ws", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error after alive-session replacement")
	}

	if oldProc.Alive() {
		t.Error("old alive process must be Close()'d during Takeover")
	}
	if _, ok := lookupT(r, key); ok {
		t.Error("old session should have been unregistered before re-spawn failed")
	}
}

// hookCloseProc is a fakeProcess whose Close() runs a hook. Takeover
// calls Close() while holding neither the table lock nor any per-session lock, so
// the hook can exercise the concurrent-creation race.
type hookCloseProc struct {
	*fakeProcess
	onClose func()
	once    sync.Once
}

func newHookCloseProc(onClose func()) *hookCloseProc {
	return &hookCloseProc{fakeProcess: newIdleProc(), onClose: onClose}
}

func (h *hookCloseProc) Close() {
	h.once.Do(func() {
		if h.onClose != nil {
			h.onClose()
		}
	})
	h.fakeProcess.Close()
}

// TestTakeover_ConcurrentCreationAborts — if another goroutine inserts a
// live session under the same key while Takeover has released the table lock to
// Close() the old process, Takeover must abort with an explicit error
// rather than silently unregister the interloper and spawn on top.
func TestTakeover_ConcurrentCreationAborts(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user4:general"

	interloper := newIdleProc()
	// onClose runs after the table lock is released by Takeover. Inject a new live
	// session under the same key to simulate a concurrent GetOrCreate
	// winning the race.
	hook := newHookCloseProc(func() {
		var s *ManagedSession
		r.ss.Update(func(tx sessTx) {
			s = &ManagedSession{key: key}
			s.storeProcess(interloper)
			s.touchLastActive()
			tx.Put(key, s)
		})
	})
	old := injectSession(r, key, hook)
	old.setSessionID("old-sess")

	_, err := r.Takeover(context.Background(), key, "new-sess", "/tmp/ws", AgentOpts{})
	if err == nil {
		t.Fatal("expected concurrent-creation abort error, got nil")
	}
	if !strings.Contains(err.Error(), "concurrent session created") {
		t.Errorf("error should identify the concurrent race, got: %v", err)
	}

	// Interloper session must survive untouched; Takeover may not
	// clobber a live parallel session.
	cur, ok := lookupT(r, key)
	if !ok {
		t.Fatal("interloper session should still be in sessions map")
	}
	if cur.loadProcess() == nil || !cur.loadProcess().Alive() {
		t.Error("interloper's live process should survive the aborted Takeover")
	}
	if !interloper.Alive() {
		t.Error("interloper's fakeProcess should not have been Close()'d by Takeover")
	}
	// The abort ends Takeover's lease: a marker left behind would park every
	// later GetOrCreate for the key forever.
	r.ss.Update(func(tx sessTx) {
		if _, inflight := tx.Ext().spawns.SpawnInFlight(key); inflight {
			t.Error("aborted Takeover left its spawn marker")
		}
	})
}

// A GetOrCreate that lands while Takeover has the lock released to close the
// old process parks on Takeover's lease and then gets Takeover's session. It
// used to find the key unmarked, spawn its own, and make Takeover abort.
func TestTakeover_ParksConcurrentGetOrCreate(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user-park:general"
	var spawns atomic.Int32
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
		spawns.Add(1)
		return newIdleProc(), nil
	}

	var got *ManagedSession
	var status SessionStatus
	getDone := make(chan struct{})
	var hook *hookCloseProc
	hook = newHookCloseProc(func() {
		// The old process is down, the session still registered: the window a
		// GetOrCreate would resume into.
		hook.fakeProcess.Close()
		go func() {
			defer close(getDone)
			got, status, _ = r.GetOrCreate(context.Background(), key, AgentOpts{})
		}()
		// The GetOrCreate either parks (right) or spawns and returns (wrong);
		// give it the chance to do the wrong thing before the close finishes.
		select {
		case <-getDone:
		case <-time.After(100 * time.Millisecond):
		}
	})
	injectSession(r, key, hook).setSessionID("old-sess")

	took, err := r.Takeover(context.Background(), key, "new-sess", t.TempDir(), AgentOpts{})
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	<-getDone
	if got != took || status != SessionExisting {
		t.Errorf("concurrent GetOrCreate got %p (status %v), want Takeover's session %p as existing", got, status, took)
	}
	if n := spawns.Load(); n != 1 {
		t.Errorf("%d spawns, want 1 (Takeover's)", n)
	}
}

// A takeover meeting a spawn already in flight refuses before touching the
// session there.
func TestTakeover_RefusesBeforeClosingDuringInFlightSpawn(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user-inflight:general"
	proc := newIdleProc()
	injectSession(r, key, proc)
	r.ss.Update(func(tx sessTx) { tx.Ext().spawns.BeginSpawn(key) })
	if _, err := r.Takeover(context.Background(), key, "new-sess", t.TempDir(), AgentOpts{}); !errors.Is(err, ErrSpawnInFlight) {
		t.Errorf("Takeover = %v, want ErrSpawnInFlight", err)
	}
	if !proc.Alive() {
		t.Error("the refused Takeover closed the live process")
	}
}

// TestTakeover_EmptyWorkspaceSkipsOverride — the guard `if chatKey != key`
// prevents writing an override for single-segment keys (or for keys where
// the caller knows no chat-scoped workspace applies). Using a key that
// equals its own chatKey exercises that guard.
func TestTakeover_EmptyWorkspaceSkipsOverride(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	// Single-segment key: chatKeyFor returns the key unchanged, so the
	// `chatKey != key` guard in Takeover must skip the override write.
	key := "singleton-key"
	if chatKeyFor(key) != key {
		t.Fatalf("test precondition: chatKeyFor(%q) should equal %q", key, key)
	}

	_, err := r.Takeover(context.Background(), key, "sess-x", "/tmp/ws", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error, got nil")
	}
	if stateOf(r).workspaces.Len() != 0 {
		t.Errorf("workspaceOverrides should remain empty for chatKey==key, got %v",
			stateOf(r).workspaces.Snapshot())
	}
	if stateOf(r).workspaces.Dirty() {
		t.Error("wsOverridesDirty should not be set when override write is skipped")
	}
}

// TestTakeover_WorkspaceOverrideIdempotent — writing the same workspace
// twice should still succeed but must not rebump wsOverridesDirty if the
// prior value already matches (the guard inside Takeover).
func TestTakeover_WorkspaceOverrideIdempotent(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user5:general"
	chatKey := chatKeyFor(key)
	// Seed = disk-loaded semantics: present, not dirty.
	stateOf(r).workspaces.Seed(map[string]string{chatKey: "/tmp/existing"})

	// Same workspace: guard should see prev == workspace and skip dirty flip.
	_, err := r.Takeover(context.Background(), key, "sess-y", "/tmp/existing", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error")
	}
	if stateOf(r).workspaces.Dirty() {
		t.Error("wsOverridesDirty should not flip when new workspace equals prior")
	}

	// Different workspace: must flip dirty.
	stateOf(r).workspaces.MarkSavedIfUnchanged(stateOf(r).workspaces.Gen())
	_, err = r.Takeover(context.Background(), key, "sess-y", "/tmp/changed", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error")
	}
	if !stateOf(r).workspaces.Dirty() {
		t.Error("wsOverridesDirty should flip when workspace changes")
	}
	if got, _ := stateOf(r).workspaces.Lookup(chatKey); got != "/tmp/changed" {
		t.Errorf("workspaceOverrides[%q] = %q, want /tmp/changed", chatKey, got)
	}
}

// compile-time sanity: hookCloseProc satisfies processIface through its
// embedded fakeProcess. If this compiles we're good.
var _ processIface = (*hookCloseProc)(nil)

// takeoverKey is a key with no session yet; sfSID's transcript is in sfWS.
const takeoverKey = "feishu:direct:bob:general"

// rejectingResumes refuses every resumed spawn the way codex / ACP do in the
// Init handshake, recording each spawn's resume ID and options.
func rejectingResumes(spawns *[]cli.SpawnOptions, fresh func() (processIface, error)) func(context.Context, cli.SpawnOptions) (processIface, error) {
	rejected := fmt.Errorf("protocol init: codex thread/resume: %w", clierr.ErrResumeRejected)
	return func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
		*spawns = append(*spawns, opts)
		if opts.ResumeID != "" {
			return nil, rejected
		}
		return fresh()
	}
}

func resumeIDs(spawns []cli.SpawnOptions) []string {
	ids := make([]string, len(spawns))
	for i, o := range spawns {
		ids[i] = o.ResumeID
	}
	return ids
}

// The external CLI is already gone when a backend refuses the resume, so the
// takeover starts fresh in the same call, chained to the adopted transcript
// and on the backend and profile the first attempt resolved.
func TestTakeover_RetriesARejectedResumeFresh(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // the retry waits on the key's shim socket
	t.Run("fresh spawn succeeds", func(t *testing.T) {
		r, _, _ := newStartupFailRouter(t, newDeadProc())
		loader := &fakeHistoryLoader{entries: []clievent.EventEntry{{Time: 1, Type: "user", Summary: "hi"}}}
		r.hist.loader = loader
		var spawns []cli.SpawnOptions
		r.spawn.hook = rejectingResumes(&spawns, func() (processIface, error) { return newIdleProc(), nil })
		s, err := r.Takeover(context.Background(), takeoverKey, sfSID, sfWS, AgentOpts{})
		if err != nil || !slices.Equal(resumeIDs(spawns), []string{sfSID, ""}) {
			t.Fatalf("Takeover err = %v, spawn resumes %q; want one rejected resume then a fresh spawn", err, resumeIDs(spawns))
		}
		if cur, ok := lookupT(r, takeoverKey); !ok || cur != s || !s.isAlive() {
			t.Fatal("the fresh session is not live at the key")
		}
		if !slices.Equal(s.prevSessionIDs, []string{sfSID}) {
			t.Errorf("prevSessionIDs = %q, want the rejected %q chained", s.prevSessionIDs, sfSID)
		}
		if !slices.Equal(loader.lastIDs, []string{sfSID}) || !s.hasInjectedHistory() {
			t.Errorf("history load ids = %q, injected %v; want the adopted transcript loaded once", loader.lastIDs, s.hasInjectedHistory())
		}
	})
	t.Run("fresh spawn fails too", func(t *testing.T) {
		r, _, _ := newStartupFailRouter(t, newDeadProc())
		var spawns []cli.SpawnOptions
		errFresh := errors.New("fresh spawn failed")
		r.spawn.hook = rejectingResumes(&spawns, func() (processIface, error) { return nil, errFresh })
		_, err := r.Takeover(context.Background(), takeoverKey, sfSID, sfWS, AgentOpts{})
		if !errors.Is(err, errFresh) || errors.Is(err, ErrShimStuck) {
			t.Fatalf("Takeover err = %v, want the fresh spawn's error", err)
		}
		if !slices.Equal(resumeIDs(spawns), []string{sfSID, ""}) {
			t.Errorf("spawn resumes = %q, want one resume then one fresh retry", resumeIDs(spawns))
		}
		if _, ok := lookupT(r, takeoverKey); ok {
			t.Error("a failed retry left a session at the key")
		}
	})
	t.Run("one-shot picks survive the retry", func(t *testing.T) {
		r, _, _ := newStartupFailRouter(t, newDeadProc())
		r.setWrappersForTest(map[string]*cli.Wrapper{
			"claude": cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude"),
			"codex":  cli.NewWrapper("/nonexistent/codex", &cli.ClaudeProtocol{}, "codex"),
		})
		setAccessProfiles(r, map[string]AccessProfile{"work": {Env: map[string]string{"PROFILE": "work"}}})
		stateOf(r).picks.backend[takeoverKey] = "codex"
		stateOf(r).picks.accessProfile[takeoverKey] = "work"
		var spawns []cli.SpawnOptions
		r.spawn.hook = rejectingResumes(&spawns, func() (processIface, error) { return newIdleProc(), nil })
		s, err := r.Takeover(context.Background(), takeoverKey, sfSID, sfWS, AgentOpts{})
		if err != nil || len(spawns) != 2 {
			t.Fatalf("Takeover err = %v after %d spawns, want a fresh retry", err, len(spawns))
		}
		if s.Backend() != "codex" || s.AccessProfile() != "work" || spawns[1].EnvOverlay["PROFILE"] != "work" {
			t.Errorf("retry ran on backend %q, profile %q, env %v; want the picked codex / work", s.Backend(), s.AccessProfile(), spawns[1].EnvOverlay)
		}
	})
	t.Run("cancelled caller is not retried", func(t *testing.T) {
		r, _, _ := newStartupFailRouter(t, newDeadProc())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var spawns []cli.SpawnOptions
		reject := rejectingResumes(&spawns, func() (processIface, error) { return newIdleProc(), nil })
		r.spawn.hook = func(ctx context.Context, opts cli.SpawnOptions) (processIface, error) {
			cancel()
			return reject(ctx, opts)
		}
		_, err := r.Takeover(ctx, takeoverKey, sfSID, sfWS, AgentOpts{})
		if !errors.Is(err, clierr.ErrResumeRejected) || len(spawns) != 1 {
			t.Fatalf("Takeover err = %v after %d spawns, want the rejection with no retry", err, len(spawns))
		}
	})
	t.Run("spawn in flight meanwhile", func(t *testing.T) {
		r, _, _ := newStartupFailRouter(t, newDeadProc())
		sock := shim.SocketPath(shim.KeyHash(takeoverKey))
		var spawns []cli.SpawnOptions
		reject := rejectingResumes(&spawns, func() (processIface, error) { return newIdleProc(), nil })
		marker := make(chan chan struct{}, 1)
		// The socket holds the retry back until, once the refused spawn ends
		// its marker, another spawn owns the key.
		r.spawn.hook = func(ctx context.Context, opts cli.SpawnOptions) (processIface, error) {
			if err := os.WriteFile(sock, nil, 0o600); err != nil {
				t.Error(err)
			}
			var first chan struct{}
			r.ss.View(func(v sessView) { first, _ = v.Ext().spawns.SpawnInFlight(takeoverKey) })
			go func() {
				<-first
				var ch chan struct{}
				r.ss.Update(func(tx sessTx) { ch, _ = tx.Ext().spawns.BeginSpawn(takeoverKey) })
				marker <- ch
				os.Remove(sock)
			}()
			return reject(ctx, opts)
		}
		_, err := r.Takeover(context.Background(), takeoverKey, sfSID, sfWS, AgentOpts{})
		ch := <-marker
		r.ss.Update(func(tx sessTx) { tx.Ext().spawns.EndSpawn(takeoverKey, ch) })
		if !errors.Is(err, ErrSpawnInFlight) || errors.Is(err, ErrShimStuck) || len(spawns) != 1 {
			t.Fatalf("Takeover err = %v after %d spawns, want ErrSpawnInFlight with no retry spawn", err, len(spawns))
		}
	})
	// A GetOrCreate woken by the refused spawn can install its session before
	// the retry reserves; the retry leaves it, and every other session, alone.
	t.Run("session installed meanwhile", func(t *testing.T) {
		r, _, _ := newStartupFailRouter(t, newDeadProc())
		bystander := injectSession(r, "feishu:direct:carol:general", newIdleProc())
		r.maxProcs = 2
		var spawns []cli.SpawnOptions
		var winner *ManagedSession
		reject := rejectingResumes(&spawns, func() (processIface, error) { return newIdleProc(), nil })
		r.spawn.hook = func(ctx context.Context, opts cli.SpawnOptions) (processIface, error) {
			if opts.ResumeID != "" {
				winner = injectSession(r, takeoverKey, newIdleProc())
			}
			return reject(ctx, opts)
		}
		_, err := r.Takeover(context.Background(), takeoverKey, sfSID, sfWS, AgentOpts{})
		if err == nil || !strings.Contains(err.Error(), "concurrent session created") || errors.Is(err, ErrShimStuck) {
			t.Errorf("Takeover err = %v, want the concurrent-session refusal", err)
		}
		if len(spawns) != 1 {
			t.Errorf("spawn resumes = %q, want no retry spawn", resumeIDs(spawns))
		}
		if cur, ok := lookupT(r, takeoverKey); !ok || cur != winner || !winner.isAlive() {
			t.Error("the session installed meanwhile was displaced")
		}
		if cur, ok := lookupT(r, bystander.key); !ok || cur != bystander || !bystander.isAlive() {
			t.Error("the retry evicted an unrelated idle session")
		}
	})
}

// Only a refused resume is retried: any other spawn error is the takeover's.
func TestTakeover_OtherSpawnErrorsAreNotRetried(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	r, _, _ := newStartupFailRouter(t, newDeadProc())
	errSpawn := errors.New("exec: no such file")
	var spawns int
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
		spawns++
		return nil, errSpawn
	}
	if _, err := r.Takeover(context.Background(), takeoverKey, sfSID, sfWS, AgentOpts{}); !errors.Is(err, errSpawn) || spawns != 1 {
		t.Fatalf("Takeover err = %v after %d spawns, want the one spawn's error", err, spawns)
	}
}

// The fresh retry waits for the refused spawn's shim to release the key's
// socket; one that outlives the wait makes a failed retry ErrShimStuck.
func TestTakeover_RejectedResumeRetryWaitsForTheSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: waits out the 2s socket-gone timeout")
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	sock := shim.SocketPath(shim.KeyHash(takeoverKey))
	errClobber := errors.New("start shim: shim already listening: refusing to clobber")
	for _, released := range []bool{true, false} {
		if err := os.WriteFile(sock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		r, _, _ := newStartupFailRouter(t, newDeadProc())
		var spawns []cli.SpawnOptions
		r.spawn.hook = rejectingResumes(&spawns, func() (processIface, error) {
			if _, err := os.Stat(sock); err == nil {
				return nil, errClobber
			}
			return newIdleProc(), nil
		})
		if released {
			time.AfterFunc(100*time.Millisecond, func() { os.Remove(sock) })
		}
		_, err := r.Takeover(context.Background(), takeoverKey, sfSID, sfWS, AgentOpts{})
		if !slices.Equal(resumeIDs(spawns), []string{sfSID, ""}) {
			t.Fatalf("released=%v: spawn resumes = %q, want one resume then one fresh retry", released, resumeIDs(spawns))
		}
		if released && err != nil {
			t.Errorf("socket released: Takeover err = %v; want the fresh retry to succeed", err)
		}
		if !released && (!errors.Is(err, ErrShimStuck) || !errors.Is(err, errClobber)) {
			t.Errorf("socket stays bound: Takeover err = %v; want ErrShimStuck wrapping the retry's error", err)
		}
		os.Remove(sock)
	}
}
