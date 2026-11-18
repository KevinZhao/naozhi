package upstream

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// fetch_sessions answers in key order, so a node whose sessions did not change
// answers the same bytes from one poll to the next and the primary's etag
// holds; map order would change the tag on almost every poll.
func TestHandleRequest_FetchSessionsStableForETag(t *testing.T) {
	r := makeRouter()
	for _, k := range []string{"feishu:direct:zed:general", "dashboard:direct:amy:general", "task:mmm", "cron:j1", "slack:group:c#t1:general"} {
		r.InjectSession(k, session.NewTestProcess())
	}
	c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(r), nil, Discovery{}, nil)
	call := func(params json.RawMessage) json.RawMessage {
		t.Helper()
		raw, err := c.handleRequest(context.Background(), context.Background(), node.ReverseMsg{Method: "fetch_sessions", Params: params}, &sync.WaitGroup{})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	full := call(nil)
	var snaps []sessionview.SessionSnapshot
	if err := json.Unmarshal(full, &snaps); err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 5 || !slices.IsSortedFunc(snaps, func(a, b sessionview.SessionSnapshot) int { return strings.Compare(a.Key, b.Key) }) {
		keys := make([]string, len(snaps))
		for i, s := range snaps {
			keys[i] = s.Key
		}
		t.Fatalf("fetch_sessions keys = %q, want 5 in key order", keys)
	}
	tag, _ := json.Marshal(map[string]string{"etag": node.ListTag(full)})
	for i := range 20 {
		if got := call(tag); string(got) != `{"not_modified":true}` {
			t.Fatalf("poll %d with the current tag = %.80s, want not_modified", i, got)
		}
	}
	r.InjectSession("task:new", session.NewTestProcess())
	if got := call(tag); string(got) == `{"not_modified":true}` {
		t.Fatal("a changed session list answered not_modified")
	}
}
