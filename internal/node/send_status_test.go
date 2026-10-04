package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// sendAnswers are node answers to a send and the status the primary passes
// on. Anything the dashboard's send_ack does not know reads as accepted, so
// a hostile or newer node cannot put an arbitrary status in front of a tab.
var sendAnswers = []struct {
	name   string
	answer string // the node's response body; "" is no body at all
	want   string
}{
	{"accepted", `{"status":"accepted"}`, "accepted"},
	{"queued", `{"status":"queued"}`, "queued"},
	{"reset", `{"status":"reset","key":"k"}`, "reset"},
	{"busy", `{"status":"busy"}`, "busy"},
	{"no body", ``, "accepted"},
	{"no status", `{"key":"k"}`, "accepted"},
	{"unknown status", `{"status":"<b>owned</b>"}`, "accepted"},
	{"error is not a status", `{"status":"error"}`, "accepted"},
	{"not json", `accepted`, "accepted"},
}

func TestReverseConn_SendPassesOnTheNodesStatus(t *testing.T) {
	for _, tc := range sendAnswers {
		t.Run(tc.name, func(t *testing.T) {
			rc, wsConn, cleanup := setupReverseConnPair(t)
			defer cleanup()

			go func() {
				wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
				var req ReverseMsg
				if err := wsConn.ReadJSON(&req); err != nil {
					return
				}
				resp := ReverseMsg{Type: "response", ReqID: req.ReqID}
				if tc.answer != "" && json.Valid([]byte(tc.answer)) {
					resp.Result = json.RawMessage(tc.answer)
				}
				wsConn.WriteJSON(resp)
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			status, err := rc.Send(ctx, "key", "/new", "")
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if status != tc.want {
				t.Errorf("status = %q, want %q", status, tc.want)
			}
		})
	}
}

func TestHTTPClient_SendPassesOnTheNodesStatus(t *testing.T) {
	for _, tc := range sendAnswers {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// A local reset answers 200, every other admission 202.
				code := http.StatusAccepted
				if tc.want == "reset" {
					code = http.StatusOK
				}
				w.WriteHeader(code)
				w.Write([]byte(tc.answer))
			}))
			defer srv.Close()

			status, err := newTestHTTPClient(t, srv, "").Send(context.Background(), "k", "/new", "")
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if status != tc.want {
				t.Errorf("status = %q, want %q", status, tc.want)
			}
		})
	}
}

// A refused send is an error with no status, never a status the primary
// could mistake for an admission.
func TestSend_RefusalIsAnErrorWithNoStatus(t *testing.T) {
	t.Run("reverse", func(t *testing.T) {
		rc, wsConn, cleanup := setupReverseConnPair(t)
		defer cleanup()
		go func() {
			wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
			var req ReverseMsg
			if err := wsConn.ReadJSON(&req); err != nil {
				return
			}
			wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Error: "session busy", Result: json.RawMessage(`{"status":"accepted"}`)})
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		status, err := rc.Send(ctx, "key", "hi", "")
		if err == nil || status != "" {
			t.Errorf("Send = (%q, %v), want (\"\", error)", status, err)
		}
	})
	t.Run("http", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"status":"accepted"}`))
		}))
		defer srv.Close()
		status, err := newTestHTTPClient(t, srv, "").Send(context.Background(), "k", "hi", "")
		if err == nil || status != "" {
			t.Errorf("Send = (%q, %v), want (\"\", error)", status, err)
		}
	})
}
