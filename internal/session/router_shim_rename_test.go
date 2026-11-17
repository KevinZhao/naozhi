package session

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// TestLookupShimTarget_RenamedSessionKeepsItsShim: a scratch promote renames
// the session while its shim keeps the scratch key it was started under. The
// reconcile tick must still see that shim as in use, or it shuts down the CLI
// the renamed session is mid-turn on ("process exited during send").
func TestLookupShimTarget_RenamedSessionKeepsItsShim(t *testing.T) {
	t.Parallel()
	const (
		scratchKey = "scratch:85f19f806dbf3c6769b567ec3ed16778:general:general"
		newKey     = "dashboard:direct:r50d790411fecf45017497662c69564b7:aside-general-97ed171fef4517a7"
		cliPID     = 4242
	)
	r := newTestRouter(3)
	proc := newRunningProc()
	proc.pid = cliPID
	injectSession(r, scratchKey, proc)
	if !r.RenameSession(scratchKey, newKey) {
		t.Fatal("RenameSession returned false")
	}

	classify := func(state shim.State) (shimTarget, shimState) {
		var tgt shimTarget
		r.ss.View(func(v sessView) { tgt = lookupShimTarget(v, state) })
		return tgt, classifyShimState(tgt.spawning, tgt.found, tgt.live, false, false)
	}

	tgt, got := classify(shim.State{Key: scratchKey, CLIPID: cliPID})
	if got != shimStateSkip {
		t.Fatalf("shim of the renamed session classified %v, want shimStateSkip", got)
	}
	if want := r.SessionFor(newKey); tgt.sess != want {
		t.Errorf("target session = %p, want the renamed session %p", tgt.sess, want)
	}

	// A shim whose CLI no session runs, or with no CLI PID recorded, is still
	// an orphan.
	for _, pid := range []int{cliPID + 1, 0} {
		if _, got := classify(shim.State{Key: scratchKey, CLIPID: pid}); got != shimStateOrphan {
			t.Errorf("CLIPID %d: classified %v, want shimStateOrphan", pid, got)
		}
	}

	// Once the renamed session's process is dead, the old-key shim is not
	// riding anything any more.
	proc.mu.Lock()
	proc.isAlive = false
	proc.mu.Unlock()
	if _, got := classify(shim.State{Key: scratchKey, CLIPID: cliPID}); got != shimStateOrphan {
		t.Errorf("dead process: classified %v, want shimStateOrphan", got)
	}
}

// After a restart the promoted session is restored with no process, so its
// shim (still under the scratch key) cannot be matched by CLI PID. It is
// matched by session ID and reconnected under the renamed key, as every other
// session's shim is, instead of being shut down as an orphan mid-turn.
func TestReconnectShims_PromotedScratchShimSurvivesRestart(t *testing.T) {
	const (
		scratchKey = "scratch:85f19f806dbf3c6769b567ec3ed16778:general:general"
		newKey     = "dashboard:direct:r50d790411fecf45017497662c69564b7:aside-general-97ed171fef4517a7"
		sid        = "0b6f3a52-6c1e-4a57-9a7e-3f1d2c4b5a69"
	)
	fs := StartFakeShimForTest(t, scratchKey, 1, []string{adoptAssistantLine})
	statePath := shim.StateFilePath(os.Getenv("XDG_RUNTIME_DIR"), shim.KeyHash(scratchKey))
	state, err := shim.ReadStateFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state.SessionID = sid
	if err := shim.WriteStateFile(statePath, state); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(storePath, []byte(`[{"key":"`+newKey+`","session_id":"`+sid+`","backend":"claude"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fs.Config
	cfg.StorePath = storePath
	r := NewRouter(cfg)
	t.Cleanup(r.Shutdown)

	r.ReconnectShimsCtx(context.Background())
	if sent := fs.Sent(); slices.Contains(sent, "shutdown") {
		t.Fatalf("the promoted session's shim was shut down as an orphan (sent %v)", sent)
	}
	fs.Attached(t)
	sess := r.SessionFor(newKey)
	if sess == nil {
		t.Fatal("renamed session missing after restore")
	}
	if p, ok := sess.loadProcess().(*cli.Process); !ok || p == nil || !p.Alive() {
		t.Fatalf("renamed session's process = %T, want the reattached shim's", sess.loadProcess())
	}
	if r.SessionFor(scratchKey) != nil {
		t.Error("reconnect published a session under the scratch key")
	}
	var owner string
	r.ss.View(func(v sessView) { owner, _ = v.KeyForID(sid) })
	if owner != newKey {
		t.Errorf("session ID indexed under %q, want %q", owner, newKey)
	}
}
