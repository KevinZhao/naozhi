package server

// Remote send with a backend and a turn ID (docs/rfc/planner.md §15.5 ②③):
// the node's dedup table, the primary's "unknown" answer, and a retry after a
// lost answer running one turn.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/wsproto"
)

func TestSendDedup(t *testing.T) {
	boom := errors.New("spawn failed")
	for _, tc := range []struct {
		name string
		// first is the first send's answer; gap is the wait before the repeat.
		firstStatus string
		firstErr    error
		gap         time.Duration
		wantRuns    int
		wantDup     bool
	}{
		{"repeat answers the first", "accepted", nil, time.Minute, 1, true},
		{"repeat of a reset", "reset", nil, time.Minute, 1, true},
		{"expired repeat runs again", "accepted", nil, sendDedupTTL + time.Second, 2, false},
		{"failed first runs again", "", boom, time.Second, 2, false},
		{"busy first runs again", "busy", nil, time.Second, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			d := newSendDedup()
			d.now = func() time.Time { return now }
			runs := 0
			send := func() (string, error) {
				runs++
				if runs == 1 {
					return tc.firstStatus, tc.firstErr
				}
				return "queued", nil
			}
			first, err := d.do(context.Background(), "k", "s1", send)
			if first.Dup || !errors.Is(err, tc.firstErr) || first.Status != tc.firstStatus {
				t.Fatalf("first = (%+v, %v)", first, err)
			}
			now = now.Add(tc.gap)
			again, err := d.do(context.Background(), "k", "s1", send)
			if err != nil || runs != tc.wantRuns || again.Dup != tc.wantDup {
				t.Fatalf("repeat = (%+v, %v) after %d runs; want %d runs, dup %v", again, err, runs, tc.wantRuns, tc.wantDup)
			}
			if tc.wantDup && again.Status != tc.firstStatus {
				t.Errorf("repeat status = %q, want the first's %q", again.Status, tc.firstStatus)
			}
			// Another key or turn ID is its own send.
			if other, _ := d.do(context.Background(), "k2", "s1", send); other.Dup {
				t.Error("the same turn ID on another key read as a repeat")
			}
		})
	}
}

// A repeat that arrives while the first is still running waits for it.
func TestSendDedup_RepeatWaitsForTheFirst(t *testing.T) {
	d := newSendDedup()
	release := make(chan struct{})
	started := make(chan struct{})
	go d.do(context.Background(), "k", "s1", func() (string, error) {
		close(started)
		<-release
		return "accepted", nil
	})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := d.do(ctx, "k", "s1", func() (string, error) { t.Error("the repeat ran"); return "", nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repeat while the first runs: err = %v, want it to wait out its ctx", err)
	}
	close(release)
	res, err := d.do(context.Background(), "k", "s1", func() (string, error) { t.Error("the repeat ran"); return "", nil })
	if err != nil || !res.Dup || res.Status != "accepted" {
		t.Fatalf("repeat after the first = (%+v, %v), want accepted dup", res, err)
	}
}

func TestSendDedup_Bounded(t *testing.T) {
	d := newSendDedup()
	for i := range sendDedupCap + 10 {
		d.do(context.Background(), "k", fmt.Sprint(i), func() (string, error) { return "accepted", nil })
	}
	if len(d.byKey) > sendDedupCap || len(d.order) > sendDedupCap {
		t.Fatalf("table holds %d / %d entries, cap %d", len(d.byKey), len(d.order), sendDedupCap)
	}
}

// lostAnswerNode is a node whose first answer is lost after the node took
// the send, as when the primary times out on a slow spawn.
type lostAnswerNode struct {
	fakeCapNode
	deliver func(context.Context, node.SendRequest) (node.SendResult, error)
	mu      sync.Mutex
	turnIDs []string
}

func (n *lostAnswerNode) Send(ctx context.Context, req node.SendRequest) (node.SendResult, error) {
	res, err := n.deliver(ctx, req)
	n.mu.Lock()
	n.turnIDs = append(n.turnIDs, req.TurnID)
	first := len(n.turnIDs) == 1
	n.mu.Unlock()
	if first {
		return node.SendResult{}, fmt.Errorf("%w: %w", node.ErrSendUnknown, context.DeadlineExceeded)
	}
	return res, err
}

// readSendAck returns the next send_ack c got.
func readSendAck(t *testing.T, c *wsClient) wsproto.SendAck {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case data := <-c.send:
			var ack wsproto.SendAck
			if json.Unmarshal(data, &ack) == nil && ack.Type == wsproto.TypeSendAck {
				return ack
			}
		case <-deadline:
			t.Fatal("no send_ack")
			return wsproto.SendAck{}
		}
	}
}

// The lost answer is "unknown" with the turn ID; the retry with the same
// send_id reaches the node, which answers it from its dedup table, so one
// turn runs. Over the reverse link (relay entry) and direct (HTTP entry with
// the Idempotency-Key header).
func TestRemoteSend_RetryAfterALostAnswerRunsOneTurn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		deliver func(h *parityHarness) func(context.Context, node.SendRequest) (node.SendResult, error)
	}{
		{"reverse", func(h *parityHarness) func(context.Context, node.SendRequest) (node.SendResult, error) {
			return h.srv.SubmitRelayed
		}},
		{"direct", func(h *parityHarness) func(context.Context, node.SendRequest) (node.SendResult, error) {
			return func(_ context.Context, req node.SendRequest) (node.SendResult, error) {
				body, _ := json.Marshal(map[string]string{"key": req.Key, "text": req.Text})
				r := httptest.NewRequest(http.MethodPost, "/api/sessions/send", bytes.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set(node.IdempotencyKeyHeader, req.TurnID)
				rec := httptest.NewRecorder()
				h.hs.sendH.handleSend(rec, r)
				var res struct {
					Status string `json:"status"`
					Dup    bool   `json:"dup"`
				}
				if rec.Code >= 400 || json.Unmarshal(rec.Body.Bytes(), &res) != nil {
					return node.SendResult{}, fmt.Errorf("node answered %d %s", rec.Code, rec.Body)
				}
				return node.SendResult{Status: res.Status, Dup: res.Dup}, nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newParityHarness(t, parityOpts{})
			turns := h.session(parityKey, false)
			nc := &lostAnswerNode{fakeCapNode: fakeCapNode{id: "remote"}, deliver: tc.deliver(h)}
			primary := newHubForTest(t, HubOptions{Router: session.NewRouter(session.RouterConfig{}), Nodes: newNodeRegistry(map[string]node.Conn{"remote": nc})}, sendEngineOpts{})
			defer primary.Shutdown()
			client := newTestWSClient()
			msg := node.ClientMsg{Type: "send", Key: parityKey, Text: "hi", Node: "remote", ID: "s0a1", SendID: "s0a1"}

			primary.handleSend(client, msg)
			if ack := readSendAck(t, client); ack.Status != "unknown" || ack.TurnID != "s0a1" || ack.ID != "s0a1" {
				t.Fatalf("first ack = %+v, want unknown with the turn ID", ack)
			}
			turns.turn(t, "the first send", okTurn("R"))

			primary.handleSend(client, msg)
			if ack := readSendAck(t, client); ack.Status != "accepted" || ack.Error != "" {
				t.Fatalf("retry ack = %+v, want accepted", ack)
			}
			h.waitEngineIdle()
			turns.noMoreTurns(t)
			if got := nc.turnIDs; len(got) != 2 || got[0] != "s0a1" || got[1] != "s0a1" {
				t.Errorf("node saw turn IDs %v, want the send_id twice", got)
			}
		})
	}
}

// recordingNode records the request it was sent.
type recordingNode struct {
	fakeCapNode
	mu  sync.Mutex
	got []node.SendRequest
}

func (n *recordingNode) Send(_ context.Context, req node.SendRequest) (node.SendResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.got = append(n.got, req)
	return node.SendResult{Status: "accepted"}, nil
}

// The WS remote send carries what a link can carry and refuses the rest
// before anything reaches the node.
func TestHub_RemoteSendPicks(t *testing.T) {
	manifest := json.RawMessage(`{"backends":[{"id":"claude"},{"id":"kiro"}],"default":"claude"}`)
	for _, tc := range []struct {
		name    string
		msg     node.ClientMsg
		want    *node.SendRequest // nil: refused
		wantErr string
	}{
		{"backend the node runs", node.ClientMsg{Backend: "kiro", ResumeID: "r1", SendID: "s1"},
			&node.SendRequest{Backend: "kiro", ResumeID: "r1", TurnID: "s1"}, ""},
		{"no picks", node.ClientMsg{}, &node.SendRequest{}, ""},
		{"backend the node lacks", node.ClientMsg{Backend: "codex"}, nil, "远程节点未启用所选后端：codex"},
		{"attachments", node.ClientMsg{FileIDs: []string{"f1"}}, nil, errRemoteFiles},
		{"access profile", node.ClientMsg{AccessProfile: "work"}, nil, ErrAccessProfileRemote.Error()},
		{"bad send_id", node.ClientMsg{SendID: "a b"}, nil, "invalid send_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc := &recordingNode{fakeCapNode: fakeCapNode{id: "remote", backends: manifest}}
			hub := newHubForTest(t, HubOptions{Router: session.NewRouter(session.RouterConfig{}), Nodes: newNodeRegistry(map[string]node.Conn{"remote": nc})}, sendEngineOpts{})
			defer hub.Shutdown()
			client := newTestWSClient()
			msg := tc.msg
			msg.Type, msg.Key, msg.Text, msg.Node, msg.ID = "send", "test:d:u:general", "hi", "remote", "r1"

			hub.handleSend(client, msg)
			ack := readSendAck(t, client)
			hub.engine.wg.Wait()
			if tc.want == nil {
				if ack.Status != "error" || ack.Error != tc.wantErr || len(nc.got) != 0 {
					t.Fatalf("ack = %+v, node got %v; want error %q and nothing sent", ack, nc.got, tc.wantErr)
				}
				return
			}
			want := *tc.want
			want.Key, want.Text = msg.Key, msg.Text
			if ack.Status != "accepted" || len(nc.got) != 1 || nc.got[0] != want {
				t.Fatalf("ack = %+v, node got %+v; want accepted and %+v", ack, nc.got, want)
			}
		})
	}
}
