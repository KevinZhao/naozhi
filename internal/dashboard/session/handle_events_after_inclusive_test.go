package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// TestHandleEvents_AfterReadmitsWatermarkMillisecond (#2456): the HTTP poll
// fallback (`fetchEvents(false)` → ?after=<lastEventTime>) must mirror the WS
// subscribe catch-up and return the entries AT the cursor ms too. A same-ms
// sibling (thinking + text from one CLI frame) appended after the client's
// last poll shares the watermark and was lost forever under the strict `>`.
// The dashboard's appendEvents drops the replayed entry by uuid.
func TestHandleEvents_AfterReadmitsWatermarkMillisecond(t *testing.T) {
	const key = "feishu:p2p:alice:general"
	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{
		MaxProcs:  4,
		StorePath: filepath.Join(t.TempDir(), "sessions.json"),
	})
	t.Cleanup(r.Shutdown)
	proc := sessionpkg.NewTestProcess()
	proc.EventLog.Append(clievent.EventEntry{Time: 1000, UUID: "old", Type: "user", Summary: "hi"})
	proc.EventLog.Append(clievent.EventEntry{Time: 2000, UUID: "a", Type: "thinking", Summary: "..."})
	r.InjectSession(key, proc)
	h := New(Deps{Router: r})

	// Client polled once and rendered "a" (cursor = 2000); the sibling lands
	// in the same millisecond afterwards.
	proc.EventLog.Append(clievent.EventEntry{Time: 2000, UUID: "b", Type: "text", Summary: "answer"})

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/events?key="+key+"&after=2000", nil)
	rec := httptest.NewRecorder()
	h.HandleEvents(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got []clievent.EventEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	uuids := make([]string, 0, len(got))
	for _, e := range got {
		uuids = append(uuids, e.UUID)
	}
	if len(got) != 2 || got[0].UUID != "a" || got[1].UUID != "b" {
		t.Fatalf("#2456 regression: after=2000 returned %v, want [a b] (same-ms sibling b must be replayed; a is dedup'd client-side)", uuids)
	}
	for _, e := range got {
		if e.UUID == "old" {
			t.Fatalf("after=2000 must not replay strictly older entries: %v", uuids)
		}
	}
}
