package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
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

// The sessions poll a primary runs from startup tells it whether a direct node
// dedups, so the first send after a restart that times out is already
// ErrSendUnknown. A node that does not declare it stays conservative, and one
// that accepts a send without the echo (downgraded) goes back to it. A send
// refused before the node's handler (auth, a proxy's 502) never echoes and
// changes nothing.
func TestHTTPClient_SessionsPollTeachesIdempotency(t *testing.T) {
	for _, tc := range []struct {
		name        string
		caps        string // CapsHeader on /api/sessions
		notModified bool   // the declaring answer is the second poll's 304
		prior       []int  // statuses of the sends answered before the checked one
		echo        bool   // the node echoes IdempotencyKeyHeader on an accepted send
		wantUnknown bool
	}{
		{"node declares", CapSendIdempotent, false, nil, false, true},
		{"declared among others", "x-future," + CapSendIdempotent, false, nil, false, true},
		{"declared on a 304", CapSendIdempotent, true, nil, false, true},
		{"node predates the header", "", false, nil, false, false},
		{"other caps only", "x-future", false, nil, false, false},
		{"declared, then a send accepted without the echo", CapSendIdempotent, false, []int{http.StatusAccepted}, false, false},
		{"declared, then a 401", CapSendIdempotent, false, []int{http.StatusUnauthorized}, false, true},
		{"declared, then a proxy's 502", CapSendIdempotent, false, []int{http.StatusBadGateway}, false, true},
		{"learned from an echo, then a 401", "", false, []int{http.StatusAccepted, http.StatusUnauthorized}, true, true},
		{"never learned, then a 503", "", false, []int{http.StatusServiceUnavailable}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/sessions" {
					conditional := r.Header.Get("If-None-Match") != ""
					if tc.caps != "" && conditional == tc.notModified {
						w.Header().Set(CapsHeader, tc.caps)
					}
					w.Header().Set("ETag", SessionsContentETagPrefix+`1"`)
					if conditional {
						w.WriteHeader(http.StatusNotModified)
						return
					}
					w.Write([]byte(`{"sessions":[]}`))
					return
				}
				if i := int(sends.Add(1)) - 1; i < len(tc.prior) {
					if tc.echo && tc.prior[i] == http.StatusAccepted {
						w.Header().Set(IdempotencyKeyHeader, r.Header.Get(IdempotencyKeyHeader))
					}
					w.WriteHeader(tc.prior[i])
					w.Write([]byte(`{"status":"accepted"}`))
					return
				}
				// Read the body first: only then does the server notice the
				// client hanging up and cancel r.Context().
				io.Copy(io.Discard, r.Body)
				<-r.Context().Done()
			}))
			defer srv.Close()

			nc := newTestHTTPClient(t, srv, "")
			polls := 1
			if tc.notModified {
				polls = 2
			}
			for range polls {
				if _, err := nc.FetchSessions(context.Background()); err != nil {
					t.Fatalf("FetchSessions: %v", err)
				}
			}
			for i, status := range tc.prior {
				_, err := nc.Send(context.Background(), SendRequest{Key: "k", Text: "hi", TurnID: "s0-" + strconv.Itoa(i)})
				if ok := status == http.StatusAccepted; (err == nil) != ok {
					t.Fatalf("answered Send %d: err = %v, want ok %v", status, err, ok)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := nc.Send(ctx, SendRequest{Key: "k", Text: "hi", TurnID: "s1"})
			if err == nil || errors.Is(err, ErrSendUnknown) != tc.wantUnknown {
				t.Fatalf("first timed-out send: err = %v, want unknown %v", err, tc.wantUnknown)
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
