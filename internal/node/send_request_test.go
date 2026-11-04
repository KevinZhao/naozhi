package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// readSendParams reads one "send" request off the node end, answers it with
// answer unless answer is "" (no answer) or "close" (the link drops), and
// hands its params on.
func readSendParams(t *testing.T, ws *websocket.Conn, answer string) <-chan map[string]string {
	t.Helper()
	got := make(chan map[string]string, 1)
	go func() {
		ws.SetReadDeadline(time.Now().Add(3 * time.Second))
		var req ReverseMsg
		if err := ws.ReadJSON(&req); err != nil {
			return
		}
		var p map[string]string
		_ = json.Unmarshal(req.Params, &p)
		got <- p
		switch answer {
		case "":
		case "close":
			ws.Close()
		default:
			ws.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: json.RawMessage(answer)})
		}
	}()
	return got
}

// A new primary puts on the wire only what the node advertised it reads; a
// pick an older node would drop is refused before anything is sent, and a
// turn ID it cannot read is left off (a plain send, as before).
func TestReverseConn_SendCarriesWhatTheNodeReads(t *testing.T) {
	full := SendRequest{Key: "k", Text: "hi", Workspace: "/w", Backend: "kiro", ResumeID: "r1", TurnID: "s1"}
	for _, tc := range []struct {
		name    string
		caps    []string
		req     SendRequest
		want    map[string]string // nil: no frame goes out
		wantErr error
	}{
		{"new node", []string{CapSendBackend, CapSendIdempotent}, full,
			map[string]string{"key": "k", "text": "hi", "workspace": "/w", "backend": "kiro", "resume_id": "r1", "turn_id": "s1"}, nil},
		{"node reads backend only", []string{CapSendBackend}, full,
			map[string]string{"key": "k", "text": "hi", "workspace": "/w", "backend": "kiro", "resume_id": "r1"}, nil},
		{"old node, backend pick", nil, full, nil, ErrSendBackendUnsupported},
		{"old node, resume pick", nil, SendRequest{Key: "k", Text: "hi", ResumeID: "r1"}, nil, ErrSendBackendUnsupported},
		{"old node, plain send", nil, SendRequest{Key: "k", Text: "hi", TurnID: "s1"},
			map[string]string{"key": "k", "text": "hi"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc, ws, cleanup := setupReverseConnPair(t)
			defer cleanup()
			rc.meta.Capabilities = capsFromSlice(tc.caps)
			got := readSendParams(t, ws, `{"status":"accepted"}`)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			res, err := rc.Send(ctx, tc.req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Send err = %v, want %v", err, tc.wantErr)
			}
			if tc.want == nil {
				select {
				case p := <-got:
					t.Fatalf("a refused send reached the node: %v", p)
				case <-time.After(100 * time.Millisecond):
				}
				return
			}
			if res.Status != "accepted" {
				t.Errorf("status = %q, want accepted", res.Status)
			}
			if p := <-got; !reflect.DeepEqual(p, tc.want) {
				t.Errorf("params = %v, want %v", p, tc.want)
			}
		})
	}
}

// A send that went out with a turn ID the node reads and got no answer is
// ErrSendUnknown; without that turn ID it is the plain error it always was.
func TestReverseConn_UnansweredSendIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name        string
		caps        []string
		how         string // "" times out, "close" drops the link
		wantUnknown bool
	}{
		{"timeout, idempotent node", []string{CapSendIdempotent}, "", true},
		{"link drop, idempotent node", []string{CapSendIdempotent}, "close", true},
		{"timeout, old node", nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc, ws, cleanup := setupReverseConnPair(t)
			defer cleanup()
			rc.meta.Capabilities = capsFromSlice(tc.caps)
			got := readSendParams(t, ws, tc.how)

			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			_, err := rc.Send(ctx, SendRequest{Key: "k", Text: "hi", TurnID: "s1"})
			<-got
			if err == nil || errors.Is(err, ErrSendUnknown) != tc.wantUnknown {
				t.Fatalf("err = %v, want unknown %v", err, tc.wantUnknown)
			}
		})
	}
}

// A direct send carries the picks in the body old nodes already read, and
// the turn ID in a header they ignore. A send that times out with the turn ID
// out is ErrSendUnknown only once the node has echoed that header, i.e. runs
// a turn ID once; for a node predating it a retry would run twice.
func TestHTTPClient_SendRequest(t *testing.T) {
	type seen struct {
		body map[string]string
		idem string
	}
	for _, tc := range []struct {
		name        string
		req         SendRequest
		echo        bool // the node echoes IdempotencyKeyHeader
		warm        bool // one answered send before the checked one
		hang        bool // the checked send gets no answer
		wantBody    map[string]string
		wantIdem    string
		wantUnknown bool
	}{
		{"picks and turn id", SendRequest{Key: "k", Text: "hi", Backend: "kiro", ResumeID: "r1", TurnID: "s1"}, true, false, false,
			map[string]string{"key": "k", "text": "hi", "backend": "kiro", "resume_id": "r1"}, "s1", false},
		{"plain", SendRequest{Key: "k", Text: "hi"}, true, false, false, map[string]string{"key": "k", "text": "hi"}, "", false},
		{"timeout, node echoed before", SendRequest{Key: "k", Text: "hi", TurnID: "s1"}, true, true, true, map[string]string{"key": "k", "text": "hi"}, "s1", true},
		{"timeout, node predates the header", SendRequest{Key: "k", Text: "hi", TurnID: "s1"}, false, true, true, map[string]string{"key": "k", "text": "hi"}, "s1", false},
		{"timeout before any answer", SendRequest{Key: "k", Text: "hi", TurnID: "s1"}, true, false, true, map[string]string{"key": "k", "text": "hi"}, "s1", false},
		{"timeout without turn id", SendRequest{Key: "k", Text: "hi"}, true, true, true, map[string]string{"key": "k", "text": "hi"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan seen, 2)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var b map[string]string
				_ = json.Unmarshal(raw, &b)
				got <- seen{body: b, idem: r.Header.Get(IdempotencyKeyHeader)}
				last := calls.Add(1) == 2 || !tc.warm
				if tc.hang && last {
					<-r.Context().Done()
					return
				}
				if k := r.Header.Get(IdempotencyKeyHeader); tc.echo && k != "" {
					w.Header().Set(IdempotencyKeyHeader, k)
				}
				w.WriteHeader(http.StatusAccepted)
				w.Write([]byte(`{"status":"queued","dup":true}`))
			}))
			defer srv.Close()

			nc := newTestHTTPClient(t, srv, "")
			if tc.warm {
				warm := tc.req
				if warm.TurnID != "" {
					warm.TurnID = "s0"
				}
				if _, err := nc.Send(context.Background(), warm); err != nil {
					t.Fatalf("warm-up Send: %v", err)
				}
				<-got
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			res, err := nc.Send(ctx, tc.req)
			s := <-got
			if !reflect.DeepEqual(s.body, tc.wantBody) || s.idem != tc.wantIdem {
				t.Errorf("node saw body %v, %s %q; want %v, %q", s.body, IdempotencyKeyHeader, s.idem, tc.wantBody, tc.wantIdem)
			}
			if tc.hang {
				if err == nil || errors.Is(err, ErrSendUnknown) != tc.wantUnknown {
					t.Fatalf("err = %v, want unknown %v", err, tc.wantUnknown)
				}
				return
			}
			if err != nil || res != (SendResult{Status: "queued", Dup: true}) {
				t.Fatalf("Send = (%+v, %v), want queued dup", res, err)
			}
		})
	}
}

func TestValidTurnID(t *testing.T) {
	for id, want := range map[string]bool{
		"":                                  false,
		"s0123456789abcdef0123456789abcdef": true,
		"a-b_c.d":                           true,
		"a b":                               false,
		"a\nb":                              false,
		string(make([]byte, MaxTurnIDLen)):  false,
	} {
		if got := ValidTurnID(id); got != want {
			t.Errorf("ValidTurnID(%q) = %v, want %v", id, got, want)
		}
	}
	long := make([]byte, MaxTurnIDLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if ValidTurnID(string(long)) {
		t.Error("an over-long turn ID passed")
	}
}
