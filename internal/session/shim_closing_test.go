package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/shim"
)

// A shim reconcile tick that lands while naozhi closes a key's process on
// purpose leaves the key's shim alone: a removal (here an Archive, as a done
// task's reap) unregisters the key before the close, and adopting its
// still-live shim then would bring the key back as a session the close
// leaves dead (shim_eof), which health judges failed and custody resumes.
func TestShimNotAdoptedDuringClose(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(r *Router, key string) bool
	}{
		{"archive", func(r *Router, key string) bool { _, ok := r.Archive(key); return ok }},
		{"remove", (*Router).Remove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRouter(4)
			const key = "task:abc123"
			state := shim.State{Key: key, Workspace: "/tmp/ws", Backend: "claude", ShimPID: 4242}
			var adopted, closing bool
			proc := newHookCloseProc(func() {
				closing = r.closing.has(key)
				_, adopted = r.adoptShimTarget(state, "claude")
			})
			injectSession(r, key, proc)
			if !tc.close(r, key) {
				t.Fatal("close refused")
			}
			if adopted || !closing {
				t.Errorf("during the close: adopted=%v closing=%v, want the shim left alone", adopted, closing)
			}
			if r.ss.Load(key) != nil {
				t.Error("the key came back as a session")
			}
			if r.closing.has(key) {
				t.Error("still closing once the close ended")
			}
			// Once the close is over a live shim is adoptable again (#1875).
			if _, adopted := r.adoptShimTarget(state, "claude"); !adopted {
				t.Error("not adopted after the close")
			}
		})
	}
}

// A stop marks the key closing for its close too, so a reconcile tick does
// not reattach the dying shim to the stopped session; its death reason stays
// "stopped", which health does not read as a failure.
func TestStopSessionMarksClosing(t *testing.T) {
	r := newTestRouter(4)
	const key = "task:abc123"
	var closing bool
	s := injectSession(r, key, newHookCloseProc(func() { closing = r.closing.has(key) }))
	if !r.StopSession(key) {
		t.Fatal("not stopped")
	}
	if !closing || r.closing.has(key) {
		t.Errorf("closing during the close=%v, after=%v; want true, false", closing, r.closing.has(key))
	}
	if got := s.Snapshot().DeathReason; got != DeathReasonStopped {
		t.Errorf("death reason = %q, want %q", got, DeathReasonStopped)
	}
}
