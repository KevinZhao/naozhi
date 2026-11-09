package session

import (
	"testing"
)

// TestHistoryLoading: a restored session reads as loading while its startup
// history load runs, and not once it is done, whether it found history or
// not.
func TestHistoryLoading(t *testing.T) {
	t.Parallel()
	var during []bool
	var sessions []*ManagedSession
	loader := &racingHistoryLoader{jsonl: historyEntries("jsonl", 2)}
	r := historyOrderRouter(t, t.TempDir(), nil, loader)
	loader.duringRead = func() {
		for _, s := range sessions {
			during = append(during, s.HistoryLoading())
		}
	}
	s := injectSession(r, "feishu:direct:u1:general", nil)
	s.setSessionID("sid-1")
	sessions = append(sessions, s)
	if s.HistoryLoading() {
		t.Fatal("loading before any loader started")
	}

	r.startBackgroundHistoryLoaders(0)
	r.hist.wg.Wait()

	if len(during) != 1 || !during[0] {
		t.Errorf("loading during the read = %v, want [true]", during)
	}
	if s.HistoryLoading() {
		t.Error("still loading after the loader finished")
	}
	if !s.hasInjectedHistory() {
		t.Error("precondition: the JSONL history was not injected")
	}
}
