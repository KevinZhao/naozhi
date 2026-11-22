package session

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/note"
)

// fakeNotes records the marks it is asked for and answers with err.
type fakeNotes struct {
	marked []string
	err    error
}

func (f *fakeNotes) List(string, time.Time) []note.Note { return nil }

func (f *fakeNotes) Useless(key, id string) (note.Note, error) {
	f.marked = append(f.marked, key+" "+id)
	if f.err != nil {
		return note.Note{}, f.err
	}
	return note.Note{ID: id, Key: key, Kind: note.KindPlanner, Text: "正文", Useless: true}, nil
}

// "没用" marks a Planner note of a local session: a key or ID that is none,
// a request naming a node (the body takes no such field), a note the
// session does not have and a gateway without side notes are each refused
// before or by the source.
func TestHandleNoteUseless(t *testing.T) {
	t.Parallel()
	const key, id = "dashboard:direct:u:general", "p0123456789ab"
	for _, c := range []struct {
		name, body string
		err        error
		noNotes    bool
		status     int
		marked     bool
	}{
		{name: "marked", body: `{"key":"` + key + `","id":"` + id + `"}`, status: http.StatusOK, marked: true},
		{name: "bad key", body: `{"key":"a\nb","id":"` + id + `"}`, status: http.StatusBadRequest},
		{name: "no key", body: `{"id":"` + id + `"}`, status: http.StatusBadRequest},
		{name: "health note id", body: `{"key":"` + key + `","id":"h1700000000000000000"}`, status: http.StatusBadRequest},
		{name: "no id", body: `{"key":"` + key + `"}`, status: http.StatusBadRequest},
		{name: "remote node", body: `{"key":"` + key + `","id":"` + id + `","node":"ec2"}`, status: http.StatusBadRequest},
		{name: "unknown field", body: `{"key":"` + key + `","id":"` + id + `","text":"x"}`, status: http.StatusBadRequest},
		{name: "empty body", body: ``, status: http.StatusBadRequest},
		{name: "no such note", body: `{"key":"` + key + `","id":"` + id + `"}`, err: note.ErrNoNote, status: http.StatusNotFound, marked: true},
		{name: "journal fails", body: `{"key":"` + key + `","id":"` + id + `"}`, err: errors.New("disk full"), status: http.StatusInternalServerError, marked: true},
		{name: "no side notes", body: `{"key":"` + key + `","id":"` + id + `"}`, noNotes: true, status: http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			src := &fakeNotes{err: c.err}
			d := Deps{Router: newFakeRouter(key)}
			if !c.noNotes {
				d.Notes = src
			}
			h := New(d)
			req := httptest.NewRequest(http.MethodPost, "/api/sessions/notes/useless", strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.HandleNoteUseless(w, req)
			if w.Code != c.status {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, c.status, w.Body.String())
			}
			if got := len(src.marked) == 1; got != c.marked {
				t.Fatalf("marked = %v, want marked %v", src.marked, c.marked)
			}
			if c.marked && src.marked[0] != key+" "+id {
				t.Fatalf("marked %q, want %q", src.marked[0], key+" "+id)
			}
			if c.status != http.StatusOK {
				if strings.Contains(w.Body.String(), "disk full") {
					t.Errorf("the response carries the internal error: %s", w.Body.String())
				}
				return
			}
			var out struct {
				Note note.Note `json:"note"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Note.ID != id || !out.Note.Useless {
				t.Fatalf("body = %s (%v), want the note marked", w.Body.String(), err)
			}
		})
	}
}
