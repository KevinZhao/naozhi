package node

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// A polled list asks with the tag of the body it holds; an unchanged answer
// returns that body again, a new body replaces it, and an error makes the
// next poll ask unconditionally.
func TestReverseConn_FetchListReusesUnchangedBody(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	body := `[{"key":"a","state":"ready"}]`
	// answer reads one request and replies; it returns the request's etag.
	answer := func(call func(context.Context) ([]map[string]any, error), resp ReverseMsg) ([]map[string]any, string, error) {
		t.Helper()
		etag := make(chan string, 1)
		go func() {
			wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
			var req ReverseMsg
			if err := wsConn.ReadJSON(&req); err != nil {
				close(etag)
				return
			}
			var p struct {
				ETag string `json:"etag"`
			}
			if len(req.Params) > 0 {
				_ = json.Unmarshal(req.Params, &p)
			}
			resp.Type, resp.ReqID = "response", req.ReqID
			wsConn.WriteJSON(resp)
			etag <- p.ETag
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		rows, err := call(ctx)
		return rows, <-etag, err
	}

	for _, fetch := range []struct {
		name string
		call func(context.Context) ([]map[string]any, error)
	}{
		{"sessions", rc.FetchSessions},
		{"projects", rc.FetchProjects},
		{"discovered", rc.FetchDiscovered},
	} {
		t.Run(fetch.name, func(t *testing.T) {
			want := []map[string]any{{"key": "a", "state": "ready"}}
			rows, etag, err := answer(fetch.call, ReverseMsg{Result: json.RawMessage(body)})
			if err != nil || etag != "" || !reflect.DeepEqual(rows, want) {
				t.Fatalf("first fetch = %v, etag %q, err %v", rows, etag, err)
			}
			rows[0]["node"] = "x" // the cache manager tags rows; the cache must not see it

			rows, etag, err = answer(fetch.call, ReverseMsg{Result: listUnchanged})
			if err != nil || etag != ListTag([]byte(body)) || !reflect.DeepEqual(rows, want) {
				t.Fatalf("unchanged fetch = %v, etag %q, err %v", rows, etag, err)
			}

			next := `[{"key":"a","state":"running"}]`
			rows, etag, err = answer(fetch.call, ReverseMsg{Result: json.RawMessage(next)})
			if err != nil || etag != ListTag([]byte(body)) || rows[0]["state"] != "running" {
				t.Fatalf("changed fetch = %v, etag %q, err %v", rows, etag, err)
			}

			if _, _, err = answer(fetch.call, ReverseMsg{Error: "boom"}); err == nil {
				t.Fatal("remote error not returned")
			}
			_, etag, err = answer(fetch.call, ReverseMsg{Result: json.RawMessage(next)})
			if err != nil || etag != "" {
				t.Fatalf("fetch after an error asked with etag %q (err %v), want none", etag, err)
			}
		})
	}
}

func TestAnswerListFetch(t *testing.T) {
	body := json.RawMessage(`[{"name":"p"}]`)
	tag, _ := json.Marshal(map[string]string{"etag": ListTag(body)})
	for _, tc := range []struct {
		name   string
		params json.RawMessage
		want   json.RawMessage
	}{
		{"no params", nil, body},
		{"matching tag", tag, listUnchanged},
		{"other tag", json.RawMessage(`{"etag":"00"}`), body},
		{"empty tag", json.RawMessage(`{"etag":""}`), body},
		{"bad params", json.RawMessage(`[1]`), body},
	} {
		if got := AnswerListFetch(tc.params, body); string(got) != string(tc.want) {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}
