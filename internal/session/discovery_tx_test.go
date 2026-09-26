package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

func tableDirty(r *Router) (dirty bool) {
	r.ss.View(func(v sessView) { dirty = v.Dirty() })
	return dirty
}

func clearDirty(r *Router) {
	r.ss.Update(func(tx sessTx) { tx.SetDirty(false) })
}

// TestClearUserLabelOrigin_MarksTheStoreDirty: handing a label back to the
// daemons is persisted, so a restart does not bring the user lock back.
func TestClearUserLabelOrigin_MarksTheStoreDirty(t *testing.T) {
	r := newTestRouter(4)
	const key = "feishu:direct:label-clear:general"
	injectLocked(r, key, newIdleProc())
	r.SetUserLabel(key, "mine")
	clearDirty(r)

	if !r.ClearUserLabelOrigin(key) {
		t.Fatal("ClearUserLabelOrigin reported the key unknown")
	}
	if !tableDirty(r) {
		t.Error("clearing the label origin left the store clean")
	}
}

// TestRegisterCronStub_LeavesALiveSessionAlone: re-registering a cron stub
// whose session is running does not rewrite the running session.
func TestRegisterCronStub_LeavesALiveSessionAlone(t *testing.T) {
	r := newTestRouter(4)
	const key = "cron:live-refresh"
	s := injectLocked(r, key, newIdleProc())
	s.setWorkspace("/srv/running")

	r.RegisterCronStub(key, "/srv/reloaded", "prompt")
	if got := s.Workspace(); got != "/srv/running" {
		t.Errorf("workspace = %q, want the running session's /srv/running", got)
	}
}

// TestTakeover_ReplacingALiveSessionKeepsTheActiveCount: the live session
// Takeover closes gives its slot to the one it spawns, so the count stays 1.
func TestTakeover_ReplacingALiveSessionKeepsTheActiveCount(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	r := spawnRouter(t, 4, func(context.Context, cli.SpawnOptions) (processIface, error) { return newIdleProc(), nil })
	const key = "feishu:direct:takeover-live:general"
	injectLocked(r, key, newIdleProc())

	if _, err := r.Takeover(context.Background(), key, "sess-external", t.TempDir(), AgentOpts{}); err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	if got := r.ss.Active(); got != 1 {
		t.Errorf("active = %d after replacing one live session, want 1", got)
	}
}

// TestSetSessionTuning_RespawnWakesAShutdownWaitingOnTheSession: closing the
// session's process for a respawn wakes a Shutdown waiting on it running.
// kiro's effort tier makes an effort change a respawn.
func TestSetSessionTuning_RespawnWakesAShutdownWaitingOnTheSession(t *testing.T) {
	r := mkTuningTestRouter(t)
	probe := &runningProbe{fakeProcess: newRunningProc(), asked: make(chan struct{})}
	addTuningSession(r, "k1", "kiro", probe)

	done := startShutdownOn(r, probe)
	if mode, err := r.SetSessionTuning(context.Background(), "k1", nil, strp("max")); err != nil || mode != TuningAppliedRespawn {
		t.Fatalf("SetSessionTuning = %q, %v; want a respawn (test premise)", mode, err)
	}
	waitShutdown(t, done, "the tuning respawn closing the running session")
}

// TestSessionPicks_CapAndRoundTrip: backend and access-profile picks read
// back what was set, a brand-new key past the cap is dropped, and a key
// already present can still be updated at the cap.
func TestSessionPicks_CapAndRoundTrip(t *testing.T) {
	r := newTestRouter(4)
	r.SetSessionAccessProfile("feishu:direct:ap:general", "work")
	if got := r.SessionAccessProfile("feishu:direct:ap:general"); got != "work" {
		t.Errorf("SessionAccessProfile = %q, want work", got)
	}
	if got := r.SessionBackend("feishu:direct:ap:general"); got != "" {
		t.Errorf("an access-profile pick leaked into the backend picks: %q", got)
	}

	for i := 0; i < maxBackendOverrides; i++ {
		r.SetSessionBackend(fmt.Sprintf("feishu:direct:cap-%d:general", i), "claude")
	}
	r.SetSessionBackend("feishu:direct:cap-over:general", "kiro")
	if got := r.SessionBackend("feishu:direct:cap-over:general"); got != "" {
		t.Errorf("a new pick past the cap was recorded: %q", got)
	}
	r.SetSessionBackend("feishu:direct:cap-0:general", "kiro")
	if got := r.SessionBackend("feishu:direct:cap-0:general"); got != "kiro" {
		t.Errorf("updating an existing pick at the cap = %q, want kiro", got)
	}
}

// TestDiscoveryExclusions_CoverTheWholeChainAndLiveWorkspaces: a session with
// a process excludes its whole ID chain from discovery, and a live one its
// workspace from the discovery scan.
func TestDiscoveryExclusions_CoverTheWholeChainAndLiveWorkspaces(t *testing.T) {
	r := newTestRouter(4)
	s := injectLocked(r, "feishu:direct:exclude:general", newIdleProc())
	s.setSessionID("sess-cur")
	s.setWorkspace("/srv/live")
	r.ss.Update(func(tx sessTx) { s.prevSessionIDs = []string{"sess-old"} })

	ids := r.DiscoveryExcludeIDs()
	if !ids["sess-cur"] || !ids["sess-old"] {
		t.Errorf("DiscoveryExcludeIDs = %v, want sess-cur and sess-old", ids)
	}
	_, sessionIDs, cwds := r.ManagedExcludeSets()
	if !sessionIDs["sess-cur"] || !cwds["/srv/live"] {
		t.Errorf("ManagedExcludeSets ids=%v cwds=%v, want sess-cur and /srv/live", sessionIDs, cwds)
	}
}

// TestRetireAutoChainOnce_MarksTheStoreDirty: a stripped chain is persisted.
func TestRetireAutoChainOnce_MarksTheStoreDirty(t *testing.T) {
	r := newTestRouter(4)
	s := injectLocked(r, "feishu:direct:retire:general", nil)
	s.historyMu.Lock()
	s.prevSessionIDs = []string{"real", "auto-x"}
	s.prevSessionOrigins = []string{"manual", "auto-spawn"}
	s.historyMu.Unlock()
	clearDirty(r)

	r.retireAutoChainOnce()
	if !tableDirty(r) {
		t.Error("retiring an auto chain left the store clean")
	}
}

// storedRouterDir writes a store with one resumable session and returns the
// store path.
func storedRouterDir(t *testing.T, key, sessionID, workspace string) string {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	s := newSessionWithID(key, sessionID)
	s.setWorkspace(workspace)
	if err := saveStoreSlice(storePath, []*ManagedSession{s}); err != nil {
		t.Fatal(err)
	}
	return storePath
}

// TestNewRouter_RestoresTheStore: construction brings back the workspace
// overrides and the sessions' history, and the orphan sweep spares the
// logs of restored sessions.
func TestNewRouter_RestoresTheStore(t *testing.T) {
	const key = "feishu:direct:restored:general"
	storePath := storedRouterDir(t, key, "sess-restored", "/srv/restored")
	if err := saveWorkspaceOverrides(storePath, map[string]string{"feishu:direct:restored": "/srv/override"}); err != nil {
		t.Fatal(err)
	}
	eventLogDir := filepath.Join(filepath.Dir(storePath), "events")
	if err := os.MkdirAll(eventLogDir, 0o700); err != nil {
		t.Fatal(err)
	}
	makeFakeLog(t, eventLogDir, key, orphanSweepAge*2)
	loader := &fakeHistoryLoader{entries: []clievent.EventEntry{{Time: 1, Type: "user", Summary: "hi"}}}

	r := NewRouter(RouterConfig{
		MaxProcs:      2,
		TTL:           time.Hour,
		StorePath:     storePath,
		EventLogDir:   eventLogDir,
		ClaudeDir:     t.TempDir(),
		HistoryLoader: loader,
	})
	t.Cleanup(r.Shutdown)
	r.historyWg.Wait() // history loaders and the orphan sweep

	if got := r.Workspace("feishu:direct:restored"); got != "/srv/override" {
		t.Errorf("Workspace = %q, want the stored override", got)
	}
	s := r.SessionFor(key)
	if s == nil {
		t.Fatal("the stored session was not restored")
	}
	if !s.hasInjectedHistory() {
		t.Error("the restored session's history was not loaded")
	}
	if _, err := os.Stat(persist.LogPath(eventLogDir, key)); err != nil {
		t.Errorf("the orphan sweep removed a restored session's log: %v", err)
	}
}
