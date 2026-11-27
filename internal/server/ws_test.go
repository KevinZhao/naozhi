package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// testCookieSecret is a fixed HMAC key for deterministic test cookie values.
var testCookieSecret = []byte("test-cookie-secret-key-for-hmac!")

func testCookieMAC(token string) string {
	if token == "" {
		return ""
	}
	mac := hmac.New(sha256.New, testCookieSecret)
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

func newTestHub(t testing.TB, token string) (*Hub, *session.Router) {
	t.Helper()
	router := session.NewRouter(session.RouterConfig{})
	hub := newHubForTest(t, HubOptions{Router: router, DashToken: token, CookieMAC: testCookieMAC(token)}, sendEngineOpts{})
	return hub, router
}

// newTestHubWithUploads is newTestHub plus a live upload store. Separate helper
// because HubOptions.UploadStore replaced Hub.SetUploadStore in #2552, and a
// hub without a store takes the "uploads not configured" branch — which is a
// different assertion than the ones about resolving file_ids.
func newTestHubWithUploads(t testing.TB, token string) (*Hub, *session.Router) {
	t.Helper()
	router := session.NewRouter(session.RouterConfig{})
	hub := newHubForTest(t, HubOptions{
		Router:      router,
		DashToken:   token,
		CookieMAC:   testCookieMAC(token),
		UploadStore: newUploadStore(),
	}, sendEngineOpts{})
	return hub, router
}

func newTestHubWithAgents(t testing.TB, token string, agents map[string]session.AgentOpts) (*Hub, *session.Router) {
	t.Helper()
	router := session.NewRouter(session.RouterConfig{})
	hub := newHubForTest(t, HubOptions{Router: router, DashToken: token, CookieMAC: testCookieMAC(token)}, sendEngineOpts{Agents: agents})
	return hub, router
}

func startWSServer(t *testing.T, hub *Hub) (string, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", hub.HandleUpgrade)
	ts := httptest.NewServer(mux)
	// The listener closes with the test even when a caller discards the
	// returned cleanup; Close is idempotent, so calling both is fine.
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	return wsURL, func() {
		hub.Shutdown()
		ts.Close()
	}
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	return conn
}

func wsWrite(t *testing.T, conn *websocket.Conn, msg node.ClientMsg) {
	t.Helper()
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

// wsRead returns the next frame that is not a workflow push loop's
// (wshub_workflow_test.go covers those; they interleave with the rest).
func wsRead(t *testing.T, conn *websocket.Conn) node.ServerMsg {
	t.Helper()
	for {
		var resp node.ServerMsg
		if err := conn.ReadJSON(&resp); err != nil {
			t.Fatalf("ws read: %v", err)
		}
		if !isWorkflowFrame(resp.Type) {
			return resp
		}
	}
}

func isWorkflowFrame(typ string) bool {
	return typ == string(wsproto.TypeWorkflowSet) || typ == string(wsproto.TypeWorkflowState)
}

// ─── Auth tests ──────────────────────────────────────────────────────────────

func TestWS_AuthNotRequired(t *testing.T) {
	hub, _ := newTestHub(t, "") // no token required
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	// Should be able to use commands without auth
	wsWrite(t, conn, node.ClientMsg{Type: "ping"})
	resp := wsRead(t, conn)

	if resp.Type != "pong" {
		t.Errorf("type = %q, want pong", resp.Type)
	}
}

func TestWS_UnauthenticatedCommandRejected(t *testing.T) {
	hub, _ := newTestHub(t, "secret")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	// Try subscribe without auth
	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	resp := wsRead(t, conn)

	if resp.Type != "error" {
		t.Errorf("type = %q, want error", resp.Type)
	}
	if !strings.Contains(resp.Error, "not authenticated") {
		t.Errorf("error = %q, want 'not authenticated'", resp.Error)
	}
}

// ─── Ping/Pong test ─────────────────────────────────────────────────────────

// TestWS_Ping_UnauthenticatedRateLimited verifies R70-SEC-MED / R71-TEST-L1:
// ping handling must consume the per-connection sendLimiter budget even for
// unauthenticated connections, so a pre-auth burst cannot amplify into
// unbounded json.Marshal + channel sends. Limiter is rate 1/s burst 5;
// sending 20 pings rapidly should produce strictly fewer than 20 pongs.
func TestWS_Ping_UnauthenticatedRateLimited(t *testing.T) {
	hub, _ := newTestHub(t, "secret-token") // token gate so connection is unauth
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	const pings = 20
	for i := 0; i < pings; i++ {
		wsWrite(t, conn, node.ClientMsg{Type: "ping"})
	}

	// Read at most 20 responses with a short deadline per read; silent drops
	// surface as a read timeout.
	pongs := 0
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	for i := 0; i < pings; i++ {
		conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
		pongs++
	}

	if pongs >= pings {
		t.Errorf("got %d pongs for %d pings, expected rate-limiter to drop some", pongs, pings)
	}
	if pongs == 0 {
		t.Errorf("got 0 pongs — expected the first burst slots (5) to succeed")
	}
}

// ─── Subscribe tests ─────────────────────────────────────────────────────────

func TestWS_SubscribeMissingKey(t *testing.T) {
	hub, _ := newTestHub(t, "")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe"})
	resp := wsRead(t, conn)

	if resp.Type != "error" {
		t.Errorf("type = %q, want error", resp.Type)
	}
}

func TestWS_SubscribeAndHistory(t *testing.T) {
	hub, router := newTestHub(t, "")
	proc := session.NewTestProcess()
	proc.EventLog.Append(clievent.EventEntry{Time: 1000, Type: "system", Summary: "init"})
	proc.EventLog.Append(clievent.EventEntry{Time: 2000, Type: "text", Summary: "hello"})
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})

	// First message: subscribed
	resp := wsRead(t, conn)
	if resp.Type != "subscribed" {
		t.Fatalf("type = %q, want subscribed", resp.Type)
	}
	if resp.Key != "test:d:u:general" {
		t.Errorf("key = %q, want test:d:u:general", resp.Key)
	}
	if resp.State == "" {
		t.Error("expected non-empty state")
	}

	// Second message: history
	resp = wsRead(t, conn)
	if resp.Type != "history" {
		t.Fatalf("type = %q, want history", resp.Type)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(resp.Events))
	}
	if resp.Events[0].Type != "system" || resp.Events[1].Type != "text" {
		t.Errorf("events = %+v", resp.Events)
	}
}

// TestWS_SubscribeHasMore: a fresh subscribe with limit=N receives only the
// newest N events, and its has_more is what mounts "load earlier" — a non-nil
// false when the whole log fits (the "no more" answer must survive
// serialization); an after-cursor catch-up is not an initial page, so it
// omits has_more and the client keeps its fallback.
func TestWS_SubscribeHasMore(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name      string
		events    int
		msg       node.ClientMsg
		wantTimes []int64
		wantMore  *bool
	}{
		{"limit keeps newest", 5, node.ClientMsg{Limit: 2}, []int64{4000, 5000}, &yes},
		{"whole log fits", 1, node.ClientMsg{Limit: 100}, []int64{1000}, &no},
		{"after cursor", 2, node.ClientMsg{After: 500}, []int64{1000, 2000}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, router := newTestHub(t, "")
			proc := session.NewTestProcess()
			for i := 1; i <= tc.events; i++ {
				proc.EventLog.Append(clievent.EventEntry{Time: int64(i * 1000), Type: "text", Summary: "msg"})
			}
			router.InjectSession("test:d:u:general", proc)
			url, cleanup := startWSServer(t, hub)
			defer cleanup()
			conn := dialWS(t, url)
			defer conn.Close()

			tc.msg.Type, tc.msg.Key = "subscribe", "test:d:u:general"
			wsWrite(t, conn, tc.msg)
			if resp := wsRead(t, conn); resp.Type != "subscribed" {
				t.Fatalf("type = %q, want subscribed", resp.Type)
			}
			resp := wsRead(t, conn)
			if resp.Type != "history" {
				t.Fatalf("type = %q, want history", resp.Type)
			}
			var times []int64
			for _, e := range resp.Events {
				times = append(times, e.Time)
			}
			if !slices.Equal(times, tc.wantTimes) {
				t.Errorf("event times = %v, want %v", times, tc.wantTimes)
			}
			if (resp.HasMore == nil) != (tc.wantMore == nil) || (resp.HasMore != nil && *resp.HasMore != *tc.wantMore) {
				t.Errorf("has_more = %v, want %v", resp.HasMore, tc.wantMore)
			}
		})
	}
}

// ─── Send tests ──────────────────────────────────────────────────────────────

func TestWS_SendAccepted(t *testing.T) {
	hub, router := newTestHubWithAgents(t, "", nil)
	proc := session.NewTestProcess()
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, node.ClientMsg{Type: "send", Key: "test:d:u:general", Text: "hello", ID: "req-1"})
	// The turn's sendTurn emits a "session_state running" broadcast on
	// the same client connection just before sessionSend completes (R236-PERF-01),
	// so it can race the send_ack into the read pipe. Skip non-target frames
	// instead of pinning a fragile "first frame" assumption.
	resp := readUntilType(t, conn, "send_ack")

	if resp.Status != "accepted" {
		t.Errorf("status = %q, want accepted", resp.Status)
	}
	if resp.ID != "req-1" {
		t.Errorf("id = %q, want req-1", resp.ID)
	}
	if resp.Key != "test:d:u:general" {
		t.Errorf("key = %q, want test:d:u:general", resp.Key)
	}
}

// TestWS_SendBroadcastsRunningToAuthenticatedClient follows a send end to end
// through the engine's notifier: the session_state "running" frame the engine
// emits must reach the authenticated client that sent it. It only arrives if
// the engine notifies the Hub's broadcaster and that broadcaster fans out over
// the Hub's own registry; a notifier wired anywhere else stays silent and the
// read below runs into dialWS's deadline.
func TestWS_SendBroadcastsRunningToAuthenticatedClient(t *testing.T) {
	hub, router := newTestHubWithAgents(t, "", nil)
	const key = "test:d:u:general"
	router.InjectSession(key, session.NewTestProcess())

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, node.ClientMsg{Type: "send", Key: key, Text: "hello", ID: "req-running"})
	for {
		resp := readUntilType(t, conn, "session_state")
		if resp.Key == key && resp.State == "running" {
			return
		}
	}
}

// readUntilType drains and discards WS frames until one matching wantType
// arrives, or fails the test on read error / 5s deadline (the same deadline
// dialWS installed via SetReadDeadline). Used by tests that exercise paths
// where multiple unordered frames may share one client connection.
func readUntilType(t *testing.T, conn *websocket.Conn, wantType string) node.ServerMsg {
	t.Helper()
	for {
		resp := wsRead(t, conn)
		if resp.Type == wantType {
			return resp
		}
	}
}

func TestWS_SendBusy(t *testing.T) {
	hub, _ := newTestHub(t, "")
	key := "test:d:u:general"

	// Another send owns the key's turn, so this one queues behind it.
	busyForTest(t, hub, key)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, node.ClientMsg{Type: "send", Key: key, Text: "hello", ID: "req-2"})
	resp := wsRead(t, conn)

	if resp.Type != "send_ack" {
		t.Fatalf("type = %q, want send_ack", resp.Type)
	}
	if resp.Status != "queued" {
		t.Errorf("status = %q, want queued", resp.Status)
	}
	if resp.ID != "req-2" {
		t.Errorf("id = %q, want req-2", resp.ID)
	}
}

// TestWS_SendRejects: a send without a key or text is an error ack, and so is
// a text past the per-field cap (R59-SEC-H1) — rather than truncated, or
// accepted and coalesced into a multi-MB CLI stdin write. A remote-node send
// enforces the same cap, before the unknown-node lookup (R62-SEC-1), so a
// dashboard user cannot bypass it by targeting a node.
func TestWS_SendRejects(t *testing.T) {
	hub, _ := newTestHub(t, "")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	big := strings.Repeat("x", maxWSSendTextBytes+1)
	for _, tc := range []struct {
		name string
		msg  node.ClientMsg
		want string
	}{
		{"missing key", node.ClientMsg{Type: "send", Text: "hello"}, ""},
		{"missing text", node.ClientMsg{Type: "send", Key: "test:d:u:general"}, ""},
		{"text too long", node.ClientMsg{Type: "send", Key: "test:d:u:general", Text: big}, "too long"},
		{"remote text too long", node.ClientMsg{Type: "send", Node: "macbook", Key: "test:d:u:general", Text: big}, "too long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := dialWS(t, url)
			defer conn.Close()
			wsWrite(t, conn, tc.msg)
			resp := wsRead(t, conn)
			if resp.Type != "send_ack" || resp.Status != "error" || !strings.Contains(resp.Error, tc.want) {
				t.Fatalf("resp = %+v, want send_ack/error %q", resp, tc.want)
			}
		})
	}
}

// ─── Client disconnect cleanup ──────────────────────────────────────────────

func TestWS_ClientDisconnectCleanup(t *testing.T) {
	hub, router := newTestHub(t, "")
	proc := session.NewTestProcess()
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)

	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	_ = wsRead(t, conn) // subscribed

	// Close connection
	conn.Close()

	// Give time for cleanup
	time.Sleep(100 * time.Millisecond)

	clientCount := registeredClients(hub)

	if clientCount != 0 {
		t.Errorf("client count = %d after disconnect, want 0", clientCount)
	}
}

// ─── Hub shutdown ────────────────────────────────────────────────────────────

func TestWS_HubShutdown(t *testing.T) {
	hub, _ := newTestHub(t, "")
	url, _ := startWSServer(t, hub)

	conn := dialWS(t, url)
	defer conn.Close()

	wsWrite(t, conn, node.ClientMsg{Type: "ping"})
	_ = wsRead(t, conn) // pong

	// Shutdown closes the live connection itself rather than waiting for the
	// client to go away, and the client sees the close.
	start := time.Now()
	hub.Shutdown()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Shutdown took %v with a client connected: it waited for the client instead of closing it", d)
	}
	if n := registeredClients(hub); n != 0 {
		t.Errorf("client count = %d after shutdown, want 0", n)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Error("the connection is still open after Shutdown")
	}
}

// ─── Integration: auth + subscribe + event push ──────────────────────────────

func TestWS_FullFlow(t *testing.T) {
	hub, router := newTestHub(t, "tok")
	proc := session.NewTestProcess()
	proc.EventLog.Append(clievent.EventEntry{Time: 1000, Type: "system", Summary: "init"})
	router.InjectSession("test:d:u:general", proc)

	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	conn := dialWS(t, url)
	defer conn.Close()

	// 1. Auth
	wsWrite(t, conn, node.ClientMsg{Type: "auth", Token: "tok"})
	resp := wsRead(t, conn)
	if resp.Type != "auth_ok" {
		t.Fatalf("auth: type = %q, want auth_ok", resp.Type)
	}

	// 2. Subscribe
	wsWrite(t, conn, node.ClientMsg{Type: "subscribe", Key: "test:d:u:general"})
	resp = wsRead(t, conn) // subscribed
	if resp.Type != "subscribed" {
		t.Fatalf("subscribe: type = %q, want subscribed", resp.Type)
	}
	resp = wsRead(t, conn) // history
	if resp.Type != "history" {
		t.Fatalf("history: type = %q, want history", resp.Type)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("history events = %d, want 1", len(resp.Events))
	}

	// 3. Event push (batched as history)
	proc.EventLog.Append(clievent.EventEntry{Time: time.Now().UnixMilli(), Type: "thinking", Summary: "reasoning..."})
	resp = wsRead(t, conn)
	if resp.Type != "history" {
		t.Fatalf("push: type = %q, want history", resp.Type)
	}
	if len(resp.Events) != 1 || resp.Events[0].Type != "thinking" {
		t.Errorf("push events: %+v", resp.Events)
	}

	// 4. Unsubscribe
	wsWrite(t, conn, node.ClientMsg{Type: "unsubscribe", Key: "test:d:u:general"})
	resp = wsRead(t, conn)
	if resp.Type != "unsubscribed" {
		t.Errorf("unsub: type = %q, want unsubscribed", resp.Type)
	}
}

// Two tabs of one user share the per-owner send budget: once it is spent, the
// second tab's first send is rate limited although its own per-connection
// limiter is untouched. The spent budget is installed after tab A's first
// send creates it, so the test does not race the 1/s refill.
func TestWS_Send_OwnerBudgetSharedAcrossTabs(t *testing.T) {
	hub, _ := newTestHub(t, "")
	url, cleanup := startWSServer(t, hub)
	defer cleanup()

	a := dialWS(t, url)
	defer a.Close()
	b := dialWS(t, url)
	defer b.Close()

	wsWrite(t, a, node.ClientMsg{Type: "send", Key: "nope", Text: "hi"})
	if resp := wsRead(t, a); resp.Error == "rate limited" {
		t.Fatal("tab A's first send was rate limited")
	}
	owners := 0
	hub.admit.sendLimiters.Range(func(k, _ any) bool {
		hub.admit.sendLimiters.Store(k, rate.NewLimiter(rate.Every(time.Hour), 0))
		owners++
		return true
	})
	if owners != 1 {
		t.Fatalf("%d owner budgets after one send, want 1", owners)
	}
	wsWrite(t, b, node.ClientMsg{Type: "send", Key: "nope", Text: "hi"})
	if resp := wsRead(t, b); resp.Error != "rate limited" {
		t.Errorf("tab B's first send = %+v, want rate limited by the shared owner budget", resp)
	}
}
