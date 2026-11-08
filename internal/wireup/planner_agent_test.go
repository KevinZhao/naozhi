package wireup

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/session"
)

// The Planner's snapshot mark relies on CompactedSince: a compaction the
// CLI reports (its stream-json frame, as the event log keeps it) after t
// counts, one before does not, and a log that does not reach back to t
// cannot rule one out.
func TestPlannerCompactedSince(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{})
	t.Cleanup(r.Shutdown)
	proc := session.NewTestProcess()
	r.InjectSession(agent.Key, proc)
	p := PlannerSessions{Router: r}

	base := time.Now().Add(-time.Hour)
	at := func(d time.Duration) int64 { return base.Add(d).UnixMilli() }
	proc.EventLog.Append(clievent.EventEntry{Type: clievent.KindText, Summary: "hi", Time: at(0)})
	if !p.CompactedSince(agent.Key, base.Add(-time.Minute)) {
		t.Error("a log starting after t ruled a compaction out")
	}
	if p.CompactedSince(agent.Key, base) {
		t.Error("compacted with none in the log")
	}

	evs, _, err := (&cli.ClaudeProtocol{}).ReadEvent(`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto","pre_tokens":967190}}`)
	if err != nil || len(evs) != 1 {
		t.Fatalf("ReadEvent = %v, %v", evs, err)
	}
	entries := cli.EventEntriesFromEventAt(evs[0], at(10*time.Minute))
	if len(entries) != 1 {
		t.Fatalf("compact_boundary kept as %d entries", len(entries))
	}
	proc.EventLog.Append(entries[0])
	if !p.CompactedSince(agent.Key, base.Add(5*time.Minute)) {
		t.Error("a compaction after t not seen")
	}
	if p.CompactedSince(agent.Key, base.Add(20*time.Minute)) {
		t.Error("a compaction before t counted")
	}
	if !p.CompactedSince("dashboard:direct:none:general", base) {
		t.Error("no session ruled a compaction out")
	}
}
