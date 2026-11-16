package inbox

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
)

type fakeSource struct{ decideErr error }

func (f *fakeSource) List(int) (open, closed []notify.Item) { return nil, nil }
func (f *fakeSource) Counts() notify.Counts                 { return notify.Counts{} }
func (f *fakeSource) Dismiss(string) error                  { return nil }
func (f *fakeSource) Decide(string, notify.Action) error    { return f.decideErr }
func (f *fakeSource) PausedUntil() time.Time                { return time.Time{} }
func (f *fakeSource) Pause(time.Time)                       {}

// An answer to an expired question is a conflict that says why, in the words
// the dashboard shows; one to a question merely closed keeps its answer.
func TestDecideExpired(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{notify.Expired{Why: "你已在会话里处理"}, "这条提问已过期（你已在会话里处理），回答没有送出"},
		{notify.ErrClosed, "item already closed"},
	} {
		h := New(&fakeSource{decideErr: c.err})
		w := httptest.NewRecorder()
		h.HandleDecide(w, httptest.NewRequest(http.MethodPost, "/api/inbox/decide", strings.NewReader(`{"id":"in_1","action":"是"}`)))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%v: %d %q, want 409 %q", c.err, w.Code, w.Body.String(), c.want)
		}
	}
}
