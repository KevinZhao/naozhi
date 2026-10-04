package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const connStateTestTimeout = 10 * time.Second

func TestConnState_BeforeStartIsNotObservable(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	if st, ok := d.ConnState(); ok {
		t.Fatalf("unstarted adapter reported %+v, want not observable", st)
	}
	if _, ok := platform.ConnStateOf(d); ok {
		t.Fatal("ConnStateOf answered for an unstarted adapter")
	}
}

// TestConnState_GatewayEventMapping drives each lifecycle handler from a
// known state, including the Disconnect discordgo emits when Stop closes the
// session.
func TestConnState_GatewayEventMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		from    platform.ConnStateKind
		stopped bool
		event   func(d *Discord)
		want    platform.ConnStateKind
	}{
		{"connect", platform.ConnConnecting, false, func(d *Discord) { d.onConnect(nil, &discordgo.Connect{}) }, platform.ConnConnected},
		{"connect after drop", platform.ConnDisconnected, false, func(d *Discord) { d.onConnect(nil, &discordgo.Connect{}) }, platform.ConnConnected},
		{"ready", platform.ConnConnecting, false, func(d *Discord) { d.onReady(nil, &discordgo.Ready{}) }, platform.ConnConnected},
		{"ready without user", platform.ConnDisconnected, false, func(d *Discord) { d.onReady(nil, nil) }, platform.ConnConnected},
		{"resumed", platform.ConnDisconnected, false, func(d *Discord) { d.onResumed(nil, &discordgo.Resumed{}) }, platform.ConnConnected},
		{"disconnect", platform.ConnConnected, false, func(d *Discord) { d.onDisconnect(nil, &discordgo.Disconnect{}) }, platform.ConnDisconnected},
		{"disconnect from stop", platform.ConnConnected, true, func(d *Discord) { d.onDisconnect(nil, &discordgo.Disconnect{}) }, platform.ConnConnected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := New(Config{BotToken: "test-token"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d.stopCtx = ctx
			if tc.stopped {
				cancel()
			}
			d.connState.Set(tc.from)
			tc.event(d)
			st, ok := d.ConnState()
			if !ok || st.State != tc.want {
				t.Fatalf("state = %q (ok=%v), want %q", st.State, ok, tc.want)
			}
		})
	}
}

func TestOnReady_StillBackfillsBotID(t *testing.T) {
	t.Parallel()
	d := New(Config{BotToken: "test-token"})
	d.onReady(nil, &discordgo.Ready{User: &discordgo.User{ID: "bot-7", Username: "naozhi"}})
	if got := d.getBotID(); got != "bot-7" {
		t.Fatalf("botID = %q, want bot-7", got)
	}
}

// TestConfigureSession_SyncEvents pins the setting, not the ordering itself:
// with discordgo's default goroutine per event, the Disconnect of a drop and
// the Connect of its reconnect race, and a late Disconnect leaves the state
// "disconnected" on a live link.
func TestConfigureSession_SyncEvents(t *testing.T) {
	t.Parallel()
	sess, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	New(Config{BotToken: "test-token"}).configureSession(sess)
	if !sess.SyncEvents {
		t.Fatal("configureSession left SyncEvents off; lifecycle handlers would race")
	}
}

// fakeGateway serves Discord's REST gateway lookup and a websocket gateway
// that answers Hello / Identify / Resume. Each websocket handshake waits for
// one value on hello, so a test can observe the state while Open is blocked.
type fakeGateway struct {
	srv        *httptest.Server
	restStatus int // non-zero: the gateway lookup fails with this status
	hello      chan struct{}
	conns      chan *websocket.Conn
	done       chan struct{}
}

func newFakeGateway(t *testing.T, restStatus int) *fakeGateway {
	t.Helper()
	g := &fakeGateway{
		restStatus: restStatus,
		hello:      make(chan struct{}, 4),
		conns:      make(chan *websocket.Conn, 4),
		done:       make(chan struct{}),
	}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	t.Cleanup(func() { close(g.done) })
	return g
}

func (g *fakeGateway) serve(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		if g.restStatus != 0 {
			http.Error(w, `{"message": "401: Unauthorized", "code": 0}`, g.restStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws://" + r.Host})
		return
	}
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	select {
	case <-g.hello:
	case <-g.done:
		return
	}
	if conn.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 45000}}) != nil {
		return
	}
	var first struct {
		Op int `json:"op"`
	}
	if conn.ReadJSON(&first) != nil {
		return
	}
	reply := map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{
		"session_id": "fake-session",
		"user":       map[string]any{"id": "bot-1", "username": "naozhi"},
	}}
	if first.Op == 6 { // Resume
		reply = map[string]any{"op": 0, "s": 2, "t": "RESUMED", "d": map[string]any{}}
	}
	if conn.WriteJSON(reply) != nil {
		return
	}
	g.conns <- conn
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// rewriteTransport sends every REST request to the fake gateway.
type rewriteTransport struct {
	target *url.URL
	next   http.RoundTripper
}

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme = rt.target.Scheme
	r.URL.Host = rt.target.Host
	r.Host = ""
	return rt.next.RoundTrip(r)
}

func newGatewayAdapter(t *testing.T, g *fakeGateway) *Discord {
	t.Helper()
	target, err := url.Parse(g.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	d := New(Config{BotToken: "test-token"})
	d.restTransport = rewriteTransport{target: target, next: g.srv.Client().Transport}
	return d
}

func waitConnState(t *testing.T, d *Discord, want platform.ConnStateKind) platform.ConnState {
	t.Helper()
	var st platform.ConnState
	testhelper.Eventually(t, func() bool {
		var ok bool
		st, ok = d.ConnState()
		return ok && st.State == want
	}, connStateTestTimeout, "discord conn state never became "+string(want))
	return st
}

// TestConnState_GatewayLifecycle runs a real discordgo session against the
// fake gateway: connecting while the first handshake is pending, connected
// after READY, disconnected once the link drops, connected again after the
// resume, and Stop leaves the last state in place.
func TestConnState_GatewayLifecycle(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t, 0)
	d := newGatewayAdapter(t, g)

	started := make(chan error, 1)
	go func() { started <- d.Start(func(context.Context, platform.IncomingMessage) {}) }()
	waitConnState(t, d, platform.ConnConnecting)
	g.hello <- struct{}{}
	if err := <-started; err != nil {
		t.Fatalf("Start: %v", err)
	}
	up := waitConnState(t, d, platform.ConnConnected)

	first := <-g.conns
	_ = first.Close()
	down := waitConnState(t, d, platform.ConnDisconnected)
	if down.Since.Before(up.Since) {
		t.Fatalf("disconnected Since %v predates connected Since %v", down.Since, up.Since)
	}
	g.hello <- struct{}{}
	<-g.conns
	back := waitConnState(t, d, platform.ConnConnected)
	if back.Since.Before(down.Since) {
		t.Fatalf("reconnected Since %v predates the drop %v", back.Since, down.Since)
	}

	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st, _ := d.ConnState(); st.State != platform.ConnConnected || !st.Since.Equal(back.Since) {
		t.Fatalf("after Stop state = %+v, want connected since %v", st, back.Since)
	}
}

// TestConnState_OpenFailureIsFailed: Start does not retry a failed Open (the
// server refuses to start), so the state is terminal and carries the error.
func TestConnState_OpenFailureIsFailed(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t, http.StatusUnauthorized)
	d := newGatewayAdapter(t, g)
	if err := d.Start(func(context.Context, platform.IncomingMessage) {}); err == nil {
		t.Fatal("Start succeeded against a gateway lookup that returns 401")
	}
	st, ok := d.ConnState()
	if !ok || st.State != platform.ConnFailed {
		t.Fatalf("state = %+v (ok=%v), want failed", st, ok)
	}
	if !strings.Contains(st.LastError, "401") || st.LastErrorAt.IsZero() {
		t.Fatalf("LastError = %q at %v, want the 401", st.LastError, st.LastErrorAt)
	}
	if strings.Contains(st.LastError, "test-token") {
		t.Fatalf("LastError leaks the bot token: %q", st.LastError)
	}
}
