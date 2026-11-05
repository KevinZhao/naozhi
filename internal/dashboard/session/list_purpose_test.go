package session

import (
	"encoding/json"
	"testing"
)

// A dispatched session carries its purpose and what it was dispatched for,
// the sidebar's second line for it; any other carries neither.
func TestHandleList_Purpose(t *testing.T) {
	r := newListRouter("task:abc", "dashboard:direct:a:general")
	h := newETagTestHandlers(t, r, noNodeAccessor{})
	h.deps.Purpose = func(key string) (string, string) {
		if key == "task:abc" {
			return "task", "iso 修登录页"
		}
		return "", ""
	}
	var body struct {
		Sessions []struct {
			Key          string `json:"key"`
			Purpose      string `json:"purpose"`
			PurposeTitle string `json:"purpose_title"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(doList(h, "").Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{"task:abc": {"task", "iso 修登录页"}, "dashboard:direct:a:general": {"", ""}}
	if len(body.Sessions) != len(want) {
		t.Fatalf("got %d sessions, want %d", len(body.Sessions), len(want))
	}
	for _, s := range body.Sessions {
		if got := [2]string{s.Purpose, s.PurposeTitle}; got != want[s.Key] {
			t.Errorf("%s = %v, want %v", s.Key, got, want[s.Key])
		}
	}
}
