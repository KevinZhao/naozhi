package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// A failed session comes back from a restart reading "ready" with no death
// reason; the health machine still holds the failure, and the list carries
// it so the dashboard keeps showing the failure and its reason.
func TestHandleList_Health(t *testing.T) {
	since := time.UnixMilli(1_700_000_000_000)
	// A background task is named once it has run past health.LongTask.
	long, short := time.Now().Add(-health.LongTask-time.Minute), time.Now().Add(-time.Minute)
	r := newListRouter("dashboard:direct:a:general", "dashboard:direct:b:general", "dashboard:direct:c:general", "dashboard:direct:d:general",
		"dashboard:direct:e:general")
	h := newETagTestHandlers(t, r, noNodeAccessor{})
	h.deps.Health = func() health.Snapshot {
		return health.Snapshot{Units: []health.Unit{
			{ID: "dashboard:direct:a:general", State: health.Failed, Evidence: health.DeathEvidence + "cli_exited_code_-1", ErrClass: health.ErrOther, Since: since},
			{ID: "dashboard:direct:b:general", State: health.Failed, Evidence: "last turn ended in an API error", ErrClass: health.ErrAuth, Since: since},
			{ID: "dashboard:direct:c:general", State: health.Idle, Since: since, Background: "sleep 3600", BackgroundSince: long},
			{ID: "dashboard:direct:e:general", State: health.Idle, Since: since, Background: "sleep 60", BackgroundSince: short},
		}}
	}
	rec := doList(h, "")
	var body struct {
		Sessions []struct {
			Key    string                     `json:"key"`
			State  string                     `json:"state"`
			Health *sessionview.SessionHealth `json:"health"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	ms := since.UnixMilli()
	want := map[string]*sessionview.SessionHealth{
		"dashboard:direct:a:general": {State: "failed", Since: ms, DeathReason: "cli_exited_code_-1", ErrClass: "other"},
		"dashboard:direct:b:general": {State: "failed", Since: ms, ErrClass: "auth"},
		"dashboard:direct:c:general": {State: "idle", Since: ms, Background: "sleep 3600", BackgroundSince: long.UnixMilli()},
		"dashboard:direct:d:general": nil,
		"dashboard:direct:e:general": {State: "idle", Since: ms},
	}
	if len(body.Sessions) != len(want) {
		t.Fatalf("got %d sessions, want %d", len(body.Sessions), len(want))
	}
	for _, s := range body.Sessions {
		if w := want[s.Key]; (w == nil) != (s.Health == nil) || w != nil && *w != *s.Health {
			t.Errorf("%s health = %+v, want %+v", s.Key, s.Health, w)
		}
		if s.State != "ready" {
			t.Errorf("%s state = %q: the process state is the router's, not the health machine's", s.Key, s.State)
		}
	}
}
