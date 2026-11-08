package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/planner/brief"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// A reply that ends between two health reconciles is never seen active, so
// the health journal holds no Active→Idle for it; the morning brief still
// lists the session as finished, from the turn's result frame, and two such
// turns are one finished session. The path is the one main wires: the
// session's read loop → the router's activity sink → the journal → the brief.
func TestBriefCountsTurnShorterThanAReconcile(t *testing.T) {
	const key = "dashboard:direct:host:general"
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })

	r, proc, emit := reconnectToFakeShim(t, key, 1, []string{adoptAssistantLine})
	r.SetActivitySink(activity.NewRecorder(j, nil))
	m := health.New(health.Config{
		Journal: j,
		Collect: func(time.Time) []health.Fact {
			return []health.Fact{{ID: key, Kind: health.KindSession, Source: "test", Coverage: health.CoverageFull}}
		},
	})
	// The last brief, a minute ago, opens the window.
	start := time.Now().Add(-time.Minute)
	data, _ := json.Marshal(brief.Brief{Day: "2000-01-01", At: start})
	if err := j.Append(journal.Entry{TS: start, Kind: journal.KindBrief, Data: data}); err != nil {
		t.Fatal(err)
	}

	m.Reconcile() // idle
	emit(`{"type":"result","subtype":"success","result":"好的","session_id":"s1"}`)
	testhelper.Eventually(t, func() bool { return !proc.AdoptedTurnPending() },
		5*time.Second, "the result never reached the read loop")
	emit(adoptAssistantLine)
	emit(`{"type":"result","subtype":"success","result":"再答一句","session_id":"s1"}`)
	m.Reconcile() // idle again: neither turn was seen running
	for _, e := range readAll(t, j) {
		if e.Kind == journal.KindHealth {
			t.Fatalf("the reconciles journaled a transition %s; the case needs none", e.Data)
		}
	}

	// The next weekday's 08:30, within a week of the window's start.
	now := time.Now()
	y, mo, d := now.Date()
	day := time.Date(y, mo, d+1, 8, 30, 0, 0, time.Local)
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, 1)
	}
	b := brief.New(brief.Config{
		Now:     func() time.Time { return day },
		Journal: j,
		Health:  m.Snapshot,
		Push:    func(notify.Push) bool { return true },
	})
	// The second result is handled on the read loop; give it time to land
	// (the brief below says whether it counted).
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if countKind(readAll(t, j), "turn") == 2 {
			break
		}
	}
	b.Check()
	got := b.Recent(1)
	if len(got) != 1 {
		t.Fatal("no brief sent: the short turns were not counted as finished")
	}
	md := got[0].Markdown
	if !strings.Contains(md, "### 完成的（1）\n- ["+key+"]") {
		t.Errorf("brief lacks the session as one finished unit:\n%s", md)
	}
	if got[0].Counts.Done != 1 {
		t.Errorf("Counts.Done = %d, want 1 (two turns of one session)", got[0].Counts.Done)
	}
}

func readAll(t *testing.T, j *journal.Journal) []journal.Entry {
	t.Helper()
	es, err := j.Read(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func countKind(es []journal.Entry, kind journal.Kind) int {
	n := 0
	for _, e := range es {
		if e.Kind == kind {
			n++
		}
	}
	return n
}
