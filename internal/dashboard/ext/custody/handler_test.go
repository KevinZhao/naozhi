package custody

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/custody"
)

type fakeSource struct {
	on     map[string]bool
	setErr error
	paused bool
	by     []string
	undone []string
}

func (f *fakeSource) Status(key string) custody.Status {
	st := custody.Status{Key: key, On: f.on[key], State: custody.StateOff, Max: custody.MaxSends}
	if st.On {
		st.State = custody.StateOn
	}
	return st
}
func (f *fakeSource) List() []custody.Status { return nil }
func (f *fakeSource) Set(key string, on bool, by string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.on[key], f.by = on, append(f.by, by)
	return nil
}
func (f *fakeSource) Paused() (bool, time.Time) { return f.paused, time.Time{} }
func (f *fakeSource) SetPaused(p bool) error    { f.paused = p; return nil }
func (f *fakeSource) Undo(key string) (string, error) {
	if f.setErr != nil {
		return "", f.setErr
	}
	f.undone = append(f.undone, key)
	return "方案: 先跑测试 (Recommended).", nil
}

func TestHandler(t *testing.T) {
	for _, c := range []struct {
		name, method, path, body string
		setErr                   error
		nilSource                bool
		code                     int
		want                     string // in the body
	}{
		{name: "list", method: "GET", path: "/api/custody", code: 200, want: `"sessions":[]`},
		{name: "one", method: "GET", path: "/api/custody?key=k", code: 200, want: `"session":{"key":"k"`},
		{name: "on", method: "PUT", path: "/api/custody", body: `{"key":"k","on":true}`, code: 200, want: `"state":"on"`},
		{name: "no on", method: "PUT", path: "/api/custody", body: `{"key":"k"}`, code: 400},
		{name: "no key", method: "PUT", path: "/api/custody", body: `{"on":true}`, code: 400},
		{name: "remote", method: "PUT", path: "/api/custody", body: `{"key":"k","node":"mini","on":true}`, code: 400, want: "远程节点"},
		{name: "local node", method: "PUT", path: "/api/custody", body: `{"key":"k","node":"local","on":true}`, code: 200},
		{name: "unknown session", method: "PUT", path: "/api/custody", body: `{"key":"k","on":true}`, setErr: custody.ErrNoSession, code: 404},
		{name: "refused", method: "PUT", path: "/api/custody", body: `{"key":"planner:main","on":true}`, setErr: custody.ErrRefused, code: 400},
		{name: "planner off", method: "PUT", path: "/api/custody", body: `{"key":"k","on":true}`, setErr: custody.ErrDisabled, code: 503, want: "Planner 没有开"},
		{name: "pause", method: "PUT", path: "/api/custody/pause", code: 200, want: `"paused":true`},
		{name: "resume", method: "DELETE", path: "/api/custody/pause", code: 200, want: `"paused":false`},
		{name: "unwired", method: "PUT", path: "/api/custody", body: `{"key":"k","on":true}`, nilSource: true, code: 503},
		{name: "unwired list", method: "GET", path: "/api/custody", nilSource: true, code: 200, want: `"paused":false`},
		{name: "undo", method: "POST", path: "/api/custody/undo", body: `{"key":"k"}`, code: 200, want: `"answer":"方案: 先跑测试 (Recommended)."`},
		{name: "undo no key", method: "POST", path: "/api/custody/undo", body: `{}`, code: 400},
		{name: "undo remote", method: "POST", path: "/api/custody/undo", body: `{"key":"k","node":"mini"}`, code: 400, want: "远程节点"},
		{name: "undo nothing", method: "POST", path: "/api/custody/undo", body: `{"key":"k"}`, setErr: custody.ErrNoSuggested, code: 404, want: "撤回"},
		{name: "undo unknown session", method: "POST", path: "/api/custody/undo", body: `{"key":"k"}`, setErr: custody.ErrNoSession, code: 404},
		{name: "undo not sent", method: "POST", path: "/api/custody/undo", body: `{"key":"k"}`, setErr: errors.New("busy"), code: 500},
		{name: "undo unwired", method: "POST", path: "/api/custody/undo", body: `{"key":"k"}`, nilSource: true, code: 503},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := &fakeSource{on: map[string]bool{}, setErr: c.setErr}
			h := New(src)
			if c.nilSource {
				h = New(nil)
			}
			mux := http.NewServeMux()
			for _, r := range h.Routes() {
				mux.HandleFunc(r.Pattern, r.Handler)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			mux.ServeHTTP(rec, req)
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
				t.Fatalf("%s %s = %d %s, want %d with %q", c.method, c.path, rec.Code, rec.Body, c.code, c.want)
			}
			if c.code == 200 && c.method == "PUT" && c.path == "/api/custody" && (len(src.by) != 1 || src.by[0] != custody.ByUser) {
				t.Errorf("set by %q, want the user", src.by)
			}
			if c.name == "undo" && (len(src.undone) != 1 || src.undone[0] != "k") {
				t.Errorf("undone %q, want k", src.undone)
			}
			if c.name == "on" {
				var st custody.Status
				if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || !st.On {
					t.Errorf("reply = %s", rec.Body)
				}
			}
		})
	}
}
