package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// fakeCronSessions is a stub CronView used to verify the dashboard's
// history filter blocks cron-spawned UUIDs without instantiating a real
// cron.Scheduler.  R245-ARCH; widened in R242-ARCH-13 (#754) to satisfy
// the consolidated CronView with no-op EnsureStub / SetJobPrompt.
type fakeCronSessions struct {
	ids map[string]bool
}

func (f fakeCronSessions) KnownSessionIDs() map[string]bool {
	out := make(map[string]bool, len(f.ids))
	for k, v := range f.ids {
		out[k] = v
	}
	return out
}

// EnsureStub is a no-op — these tests never exercise stub revival.
// Returning false matches the "no matching cron job" contract so a
// caller branching on the return value sees consistent fake behaviour.
func (f fakeCronSessions) EnsureStub(string) bool { return false }

// SetJobPrompt is a no-op — these tests never auto-save the first prompt.
// Returning nil keeps the err==nil happy path identical to a fresh job.
func (f fakeCronSessions) SetJobPrompt(string, string) error { return nil }

// makeProjectDir creates a project directory under claudeDir that
// resolves back to a real workspace, plus a UUID JSONL inside it.
// The encoded directory name follows Claude Code's path encoding
// (slashes replaced with hyphens).
func makeProjectDir(t *testing.T, claudeDir string, sessionID string) (workspace, encodedDir string) {
	t.Helper()
	workspace = filepath.Join(t.TempDir(), "wsproj")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	encodedDir = strings.ReplaceAll(workspace, "/", "-")
	projDir := filepath.Join(claudeDir, "projects", encodedDir)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Real-looking JSONL with mtime in the recent window.
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

// TestLoadHistorySessions_HidesCronSessionIDs verifies that a session
// ID known to the cron Scheduler is filtered out of the history panel
// even though its JSONL still lives in ~/.claude/projects.
func TestLoadHistorySessions_HidesCronSessionIDs(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.sessionH.WaitWarmHistory()

	// Replace the runtime claudeDir with a fresh temp dir so the test
	// data is the only history available.
	claudeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(claudeDir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv.sessionH.claudeDir = claudeDir

	// One visible user session, one cron-spawned session.
	visibleSID := "11111111-2222-3333-4444-000000000001"
	cronSID := "11111111-2222-3333-4444-0000cccc0001"
	makeProjectDir(t, claudeDir, visibleSID)
	makeProjectDir(t, claudeDir, cronSID)

	// Inject the stub cron lister.
	srv.sessionH.cronSessions = fakeCronSessions{ids: map[string]bool{cronSID: true}}

	// Reset cache so the load triggers a real FS scan.
	srv.sessionH.historyCacheMu.Lock()
	srv.sessionH.historyCache = nil
	srv.sessionH.historyCacheTime = time.Time{}
	srv.sessionH.historyCacheMu.Unlock()

	got := srv.sessionH.loadHistorySessions()

	var sawVisible, sawCron bool
	for _, rs := range got {
		if rs.SessionID == visibleSID {
			sawVisible = true
		}
		if rs.SessionID == cronSID {
			sawCron = true
		}
	}
	if !sawVisible {
		t.Errorf("regular user session was filtered out: %s; got=%+v", visibleSID, got)
	}
	if sawCron {
		t.Errorf("cron-known session leaked into history: %s; got=%+v", cronSID, got)
	}
}

// TestLoadHistorySessions_HidesSysWorkspace verifies that a session
// whose workspace matches sysWorkDir (= sysession Runner cwd) is
// hidden — AutoTitler's transient claude -p subprocesses live there.
func TestLoadHistorySessions_HidesSysWorkspace(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.sessionH.WaitWarmHistory()

	claudeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(claudeDir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv.sessionH.claudeDir = claudeDir

	sysSID := "ffffffff-1111-2222-3333-000000000099"
	sysWS, _ := makeProjectDir(t, claudeDir, sysSID)

	srv.sessionH.sysWorkDir = sysWS

	srv.sessionH.historyCacheMu.Lock()
	srv.sessionH.historyCache = nil
	srv.sessionH.historyCacheTime = time.Time{}
	srv.sessionH.historyCacheMu.Unlock()

	got := srv.sessionH.loadHistorySessions()
	for _, rs := range got {
		if rs.SessionID == sysSID {
			t.Errorf("sys-sessions JSONL leaked into history (workspace=%s): %+v", sysWS, rs)
		}
	}
}

// TestHistoryFilter_NilCronSessionsDegrades asserts the historyFilter
// scaffolding tolerates a nil CronView (e.g. tests / configs where cron
// is disabled) — must not panic and must not over-filter.
func TestHistoryFilter_NilCronSessionsDegrades(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.sessionH.WaitWarmHistory()

	claudeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(claudeDir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv.sessionH.claudeDir = claudeDir
	srv.sessionH.cronSessions = nil
	srv.sessionH.sysWorkDir = ""

	visibleSID := "ffffffff-eeee-dddd-cccc-000000000777"
	makeProjectDir(t, claudeDir, visibleSID)

	srv.sessionH.historyCacheMu.Lock()
	srv.sessionH.historyCache = nil
	srv.sessionH.historyCacheTime = time.Time{}
	srv.sessionH.historyCacheMu.Unlock()

	got := srv.sessionH.loadHistorySessions()
	var saw bool
	for _, rs := range got {
		if rs.SessionID == visibleSID {
			saw = true
		}
	}
	if !saw {
		t.Errorf("with nil cronSessions and empty sysWorkDir, regular session must remain visible: %s", visibleSID)
	}
}

// TestDashboardListingFilter_ContractRoundtrip pins the per-prefix skip
// decision the dashboard's sidebar listing path applies (see
// dashboard_session.go::listSessions): cron / sys / scratch namespaces
// must be hidden from the catch-all "recent sessions" sidebar; standard
// IM keys and project planner keys (deliberately surfaced via the
// project sidebar grouping) must remain visible.
//
// Replaces the prior contract that pinned session.IsUserVisibleKey,
// which was deleted because no listing path actually consulted it —
// project keys could not flow through that umbrella without regressing
// the planner UI. See #1212 for the deletion rationale.
func TestDashboardListingFilter_ContractRoundtrip(t *testing.T) {
	hiddenInSidebar := func(key string) bool {
		return sessionkey.IsScratchKey(key) || sessionkey.IsCronKey(key) || sessionkey.IsSysKey(key)
	}
	for _, k := range []string{"cron:job-1", "sys:auto-titler", "scratch:abc:general:general"} {
		if !hiddenInSidebar(k) {
			t.Errorf("dashboard listing must hide %q", k)
		}
	}
	for _, k := range []string{"feishu:group:c:general", "slack:direct:U:general", "project:myrepo:planner"} {
		if hiddenInSidebar(k) {
			t.Errorf("dashboard listing must surface %q", k)
		}
	}

	// Avoid unused-import surface if discovery is later removed from this file.
	_ = discovery.RecentSession{}
}
