package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// stubHistorySource returns entries strictly older than beforeMS, newest
// `limit` of them, or err.
type stubHistorySource struct {
	all []clievent.EventEntry
	err error
}

func (s stubHistorySource) LoadBefore(_ context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []clievent.EventEntry
	for _, e := range s.all {
		if e.Time < beforeMS {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// The local before= page always carries X-Events-Has-More, and a page cut
// short by a disk error or a cancelled request reports "1" so the dashboard
// does not disable "load earlier" on a read that never reached the bottom.
func TestHandleEvents_LocalBeforeSetsHasMore(t *testing.T) {
	cases := []struct {
		name  string
		src   history.Source
		ctx   context.Context
		query string
		want  string
	}{
		{name: "disk error", src: stubHistorySource{err: errors.New("disk read failed")}, query: "&before=3000&limit=10", want: "1"},
		{name: "clean exhaustion", src: stubHistorySource{all: []clievent.EventEntry{{Time: 500, UUID: "old", Type: "text"}}}, query: "&before=3000&limit=10", want: "0"},
		{name: "older on disk", src: stubHistorySource{all: []clievent.EventEntry{{Time: 500, UUID: "old", Type: "text"}}}, query: "&before=3000&limit=2", want: "1"},
		{name: "cancelled request", src: stubHistorySource{}, ctx: cancelledRequestCtx(), query: "&before=3000&limit=10", want: "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const key = "feishu:p2p:alice:general"
			r := sessionpkg.NewRouter(sessionpkg.RouterConfig{MaxProcs: 4, StorePath: filepath.Join(t.TempDir(), "sessions.json")})
			t.Cleanup(r.Shutdown)
			proc := sessionpkg.NewTestProcess()
			for _, e := range wireFixture() {
				proc.EventLog.Append(e)
			}
			r.InjectSession(key, proc)
			r.SessionFor(key).SetHistorySource(tc.src)
			h := New(Deps{Router: realRouter{r}})

			req := httptest.NewRequest(http.MethodGet, "/api/sessions/events?key="+key+tc.query, nil)
			if tc.ctx != nil {
				req = req.WithContext(tc.ctx)
			}
			rec := httptest.NewRecorder()
			h.HandleEvents(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got, ok := rec.Header()["X-Events-Has-More"]; !ok || len(got) != 1 || got[0] != tc.want {
				t.Errorf("X-Events-Has-More=%q (present=%v) want %q; body=%s", got, ok, tc.want, rec.Body.String())
			}
		})
	}
}

func cancelledRequestCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
