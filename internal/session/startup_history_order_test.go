package session

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/shim"
)

// eventLogView is a naozhi event-log history with the tool and thinking turns
// a Claude JSONL tail does not carry.
func eventLogView(prefix string) []clievent.EventEntry {
	types := []string{"user", "thinking", "tool_use", "text"}
	out := make([]clievent.EventEntry, len(types))
	for i, typ := range types {
		out[i] = clievent.EventEntry{UUID: fmt.Sprintf("%s-u%d", prefix, i), Time: int64(500 * (i + 1)), Type: typ, Summary: fmt.Sprintf("%s-%d", prefix, i)}
	}
	return out
}

// writeEventLog persists entries as key's naozhi event log through p.
func writeEventLog(t *testing.T, p *persist.Persister, key string, entries []clievent.EventEntry) {
	t.Helper()
	sink := p.SinkFor(key)
	for _, e := range entries {
		js, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		sink([]persist.Entry{{JSON: js, TimeMS: e.Time}}, false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

// historyOrderRouter is a Router with an event log under dir, a Claude dir and
// loader as its JSONL source, and no restored sessions.
func historyOrderRouter(t *testing.T, dir string, w *cli.Wrapper, loader HistoryLoader) *Router {
	t.Helper()
	r := NewRouter(RouterConfig{
		Wrapper: w, ClaudeDir: filepath.Join(dir, "claude"), EventLogDir: filepath.Join(dir, "events"), HistoryLoader: loader,
	})
	t.Cleanup(r.Shutdown)
	if r.hist.persister == nil {
		t.Fatal("precondition: EventLogDir did not start a persister")
	}
	return r
}

// TestReconnectShims_RestoresEventLogBeforeJSONL: both shim paths restore a
// session from its naozhi event log when it has rows, without reading the
// Claude JSONL, so the race with the startup loaders cannot leave the
// session with the Claude-only view.
func TestReconnectShims_RestoresEventLogBeforeJSONL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim discovery needs unix PID liveness and unix sockets")
	}
	for _, tc := range []struct {
		name  string
		drift bool
	}{{"reconnect", false}, {"drift backfill", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortTempDir(t)
			mgr, err := shim.NewManager(shim.ManagerConfig{StateDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			w := cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")
			w.ShimManager = mgr
			loader := &racingHistoryLoader{jsonl: historyEntries("jsonl", 3)}
			r := historyOrderRouter(t, dir, w, loader)

			key := "feishu:direct:alice:general"
			sess := injectSession(r, key, nil)
			logView := eventLogView("log")
			writeEventLog(t, r.hist.persister, key, logView)
			if tc.drift {
				writeDriftedShim(t, dir, r, w, sess)
			} else {
				writeLiveShim(t, dir, r, w, sess)
			}

			r.ReconnectShimsCtx(context.Background())

			if tc.drift != r.drift.has(key) {
				t.Fatalf("precondition: drifted = %v, want %v", r.drift.has(key), tc.drift)
			}
			if got := loader.calls.Load(); got != 0 {
				t.Errorf("JSONL reads = %d, want 0: the event log had rows", got)
			}
			want := fmt.Sprint(summariesOf(logView))
			if got := persistedSummaries(t, sess); fmt.Sprint(got) != want {
				t.Errorf("persistedHistory = %v, want the event-log view %v", got, want)
			}
			if !tc.drift {
				if got := summariesOf(sess.EventEntries()); fmt.Sprint(got) != want {
					t.Errorf("reattached process event log = %v, want %v", got, want)
				}
			}
		})
	}
}

// TestStartupHistory_EventLogBeforeJSONL: the startup loader reads a
// session's JSONL only when its event log has no rows, so a session with both
// sources always gets the event-log view, and not for a session a shim
// reconnect already filled.
func TestStartupHistory_EventLogBeforeJSONL(t *testing.T) {
	t.Parallel()
	const withLog, withoutLog, filled = 16, 4, 2
	loader := &racingHistoryLoader{jsonl: historyEntries("jsonl", 3)}
	r := historyOrderRouter(t, t.TempDir(), nil, loader)
	var logged, unlogged, prefilled []*ManagedSession
	for i := range withLog + withoutLog + filled {
		key := fmt.Sprintf("feishu:direct:u%d:general", i)
		s := injectSession(r, key, nil)
		s.setSessionID(fmt.Sprintf("sid-%d", i))
		switch {
		case i < withLog:
			writeEventLog(t, r.hist.persister, key, eventLogView(key))
			logged = append(logged, s)
		case i < withLog+withoutLog:
			unlogged = append(unlogged, s)
		default:
			s.InjectHistory(historyEntries("shim", 2))
			prefilled = append(prefilled, s)
		}
	}

	r.startBackgroundHistoryLoaders()
	r.hist.wg.Wait()

	if got := loader.calls.Load(); got != withoutLog {
		t.Errorf("JSONL reads = %d, want %d: one per session without event-log rows", got, withoutLog)
	}
	for _, s := range logged {
		if got, want := persistedSummaries(t, s), summariesOf(eventLogView(s.key)); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: persistedHistory = %v, want the event-log view %v", s.key, got, want)
		}
	}
	for _, s := range unlogged {
		if got, want := persistedSummaries(t, s), summariesOf(loader.jsonl); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: persistedHistory = %v, want the JSONL tail %v", s.key, got, want)
		}
	}
	for _, s := range prefilled {
		if got, want := persistedSummaries(t, s), summariesOf(historyEntries("shim", 2)); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: persistedHistory = %v, want the history it held %v", s.key, got, want)
		}
	}
}

// TestRestoredHistory_ConcurrentInjectorsInjectOnce runs the startup loader
// and eight shim-reconnect injects for one session at once. One of them
// injects the event-log view; the rest neither append nor fall back to JSONL.
func TestRestoredHistory_ConcurrentInjectorsInjectOnce(t *testing.T) {
	t.Parallel()
	loader := &racingHistoryLoader{jsonl: historyEntries("jsonl", 3)}
	r := historyOrderRouter(t, t.TempDir(), nil, loader)
	const key = "feishu:direct:alice:general"
	s := injectSession(r, key, nil)
	s.setSessionID("sid-1")
	logView := eventLogView("log")
	writeEventLog(t, r.hist.persister, key, logView)

	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			if r.hist.injectRestoredHistory(context.Background(), s, []string{"sid-1"}, restoreViaShimReconnect) {
				wins.Add(1)
			}
		})
	}
	r.startBackgroundHistoryLoaders()
	close(start)
	wg.Wait()
	r.hist.wg.Wait()

	if got := wins.Load(); got > 1 {
		t.Errorf("%d shim injects won, want at most 1", got)
	}
	if got := loader.calls.Load(); got != 0 {
		t.Errorf("JSONL reads = %d, want 0: the event log had rows", got)
	}
	if got, want := persistedSummaries(t, s), summariesOf(logView); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("persistedHistory = %v, want the event-log view once %v", got, want)
	}
}

// TestRestoredHistory_ShimInjectFillsPersistGaps: an event-log inject from
// the shim path reads the persist_gap fill as the startup loader's does, so
// the dropped q2..q4 are served and the twins of stored turns are not.
func TestRestoredHistory_ShimInjectFillsPersistGaps(t *testing.T) {
	t.Parallel()
	const key = "feishu:p2p:gapfill-shim"
	fb := &blockingFallback{release: make(chan struct{})}
	close(fb.release)
	for i := 1; i <= 5; i++ {
		fb.entries = append(fb.entries, clievent.EventEntry{UUID: fmt.Sprintf("c%d", i), Time: int64(i*1000 + 5), Type: "user", Detail: fmt.Sprintf("q%d", i)})
	}
	r, s := gapLoaderRouter(t, key, []string{
		`{"uuid":"l1","time":1000,"type":"user","detail":"q1"}`,
		`{"time":5000,"type":"persist_gap","detail":"dropped=3"}`,
		`{"uuid":"l5","time":5000,"type":"user","detail":"q5"}`,
	}, []int64{1000, 5000, 5000}, fb)

	if !r.hist.injectRestoredHistory(context.Background(), s, nil, restoreViaShimReconnect) {
		t.Fatal("injectRestoredHistory into an empty session reported no inject")
	}

	page, _ := s.EventInitialPageCtx(context.Background(), DefaultVisibleTarget, 0)
	got := map[string]int{}
	for _, e := range page {
		got[e.UUID]++
	}
	for _, u := range []string{"l1", "l5", "c2", "c3", "c4"} {
		if got[u] != 1 {
			t.Errorf("%s served %d times on the first page, want 1; page %v", u, got[u], got)
		}
	}
	if got["c1"] > 0 || got["c5"] > 0 {
		t.Errorf("a twin of a local turn leaked: %v", got)
	}
}
