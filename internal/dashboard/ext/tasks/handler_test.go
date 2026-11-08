package tasks

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/planner/task"
)

// fakeSource records what the handler asked of the board.
type fakeSource struct {
	list    []task.Task
	sets    []string
	stops   []string
	seenIDs [][]string
	seenAll []bool
	seenN   int
	err     error
}

func (f *fakeSource) List() []task.Task { return f.list }

func (f *fakeSource) Set(id string, state task.State, by task.By, _ string) error {
	f.sets = append(f.sets, id+"="+string(state)+"/"+string(by))
	return f.err
}

func (f *fakeSource) Stop(id string) error {
	f.stops = append(f.stops, id)
	return f.err
}

func (f *fakeSource) Seen(ids []string, all bool) (int, error) {
	f.seenIDs = append(f.seenIDs, ids)
	f.seenAll = append(f.seenAll, all)
	return f.seenN, f.err
}

func do(h http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(method, "/", strings.NewReader(body)))
	return w
}

// GET lists the board's tasks with their new flag; an empty board is [].
func TestHandleList(t *testing.T) {
	src := &fakeSource{list: []task.Task{{ID: "task:a", Kind: task.KindSession, State: task.Done, New: true}}}
	w := do(New(src).HandleList, http.MethodGet, "")
	var got struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("list = %d %s (%v)", w.Code, w.Body, err)
	}
	if len(got.Tasks) != 1 || got.Tasks[0]["new"] != true {
		t.Errorf("list = %s", w.Body)
	}
	for _, h := range []*Handler{New(&fakeSource{}), New(nil)} {
		if w := do(h.HandleList, http.MethodGet, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"tasks":[]}` {
			t.Errorf("empty list = %d %s", w.Code, w.Body)
		}
	}
}

// POST /api/tasks/state takes doing or done as the user's; /stop stops.
func TestHandleStateAndStop(t *testing.T) {
	src := &fakeSource{}
	h := New(src)
	if w := do(h.HandleState, http.MethodPost, `{"id":"task:a","state":"done"}`); w.Code != 200 {
		t.Fatalf("state = %d %s", w.Code, w.Body)
	}
	if w := do(h.HandleState, http.MethodPost, `{"id":"task:a","state":"failed"}`); w.Code != 400 {
		t.Errorf("state failed = %d, want 400", w.Code)
	}
	if w := do(h.HandleStop, http.MethodPost, `{"id":"task:a"}`); w.Code != 200 {
		t.Fatalf("stop = %d %s", w.Code, w.Body)
	}
	if !slices.Equal(src.sets, []string{"task:a=done/user"}) || !slices.Equal(src.stops, []string{"task:a"}) {
		t.Errorf("sets %v, stops %v", src.sets, src.stops)
	}
	src.err = task.ErrNotFound
	if w := do(h.HandleStop, http.MethodPost, `{"id":"task:x"}`); w.Code != 404 {
		t.Errorf("stop unknown = %d, want 404", w.Code)
	}
	if w := do(New(nil).HandleState, http.MethodPost, `{"id":"task:a","state":"done"}`); w.Code != 503 {
		t.Errorf("state without a board = %d, want 503", w.Code)
	}
}

// POST /api/tasks/seen takes ids or all, never both nor neither, and says
// how many it marked.
func TestHandleSeen(t *testing.T) {
	src := &fakeSource{seenN: 2}
	h := New(src)
	w := do(h.HandleSeen, http.MethodPost, `{"ids":["task:a","task:b"]}`)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"status":"ok","seen":2}` {
		t.Fatalf("seen ids = %d %s", w.Code, w.Body)
	}
	if w := do(h.HandleSeen, http.MethodPost, `{"all":true}`); w.Code != 200 {
		t.Fatalf("seen all = %d %s", w.Code, w.Body)
	}
	if len(src.seenIDs) != 2 || !slices.Equal(src.seenIDs[0], []string{"task:a", "task:b"}) || src.seenAll[0] ||
		len(src.seenIDs[1]) != 0 || !src.seenAll[1] {
		t.Errorf("Seen calls: ids %v all %v", src.seenIDs, src.seenAll)
	}
	many := `{"ids":[` + strings.TrimSuffix(strings.Repeat(`"task:a",`, maxSeenIDs+1), ",") + `]}`
	for _, body := range []string{`{}`, `{"ids":[]}`, `{"all":false}`, `{"ids":["task:a"],"all":true}`, `{"id":"task:a"}`, `not json`, many} {
		if w := do(h.HandleSeen, http.MethodPost, body); w.Code != 400 {
			t.Errorf("seen %.40s = %d, want 400", body, w.Code)
		}
	}
	if len(src.seenIDs) != 2 {
		t.Errorf("a refused request reached the board: %v", src.seenIDs)
	}
	src.err = errors.New("disk full")
	if w := do(h.HandleSeen, http.MethodPost, `{"all":true}`); w.Code != 500 {
		t.Errorf("seen failing = %d, want 500", w.Code)
	}
	if w := do(New(nil).HandleSeen, http.MethodPost, `{"all":true}`); w.Code != 503 {
		t.Errorf("seen without a board = %d, want 503", w.Code)
	}
}

// The routes are the four, as routes.golden.json lists them.
func TestRoutes(t *testing.T) {
	var got []string
	for _, r := range New(nil).Routes() {
		got = append(got, r.Pattern)
	}
	want := []string{"GET /api/tasks", "POST /api/tasks/state", "POST /api/tasks/stop", "POST /api/tasks/seen"}
	if !slices.Equal(got, want) {
		t.Errorf("routes = %v", got)
	}
}
