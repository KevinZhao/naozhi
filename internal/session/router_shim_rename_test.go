package session

import (
	"testing"

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
