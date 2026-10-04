package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const connStateTestTimeout = 5 * time.Second

func TestConnState_WebhookModeIsNotObservable(t *testing.T) {
	t.Parallel()
	f := &Feishu{mode: "webhook"}
	// Even a tracker that somehow holds a state must not be served for a
	// transport that has no connection.
	f.connState.Set(platform.ConnConnected)
	if s, ok := f.ConnState(); ok {
		t.Fatalf("webhook mode reported %+v, want not observable", s)
	}
}

func TestConnState_WebsocketBeforeStartIsNotObservable(t *testing.T) {
	t.Parallel()
	f := &Feishu{mode: "websocket"}
	if s, ok := f.ConnState(); ok {
		t.Fatalf("unstarted websocket reported %+v, want not observable", s)
	}
}

// wsGateway is a fake Feishu long-connection service: the endpoint lookup is
// scripted one reply per call, so the test can observe the adapter's state
// while the SDK is parked between lifecycle hooks.
type wsGateway struct {
	srv      *httptest.Server
	fetching chan struct{} // one send per endpoint lookup, before it replies
	script   chan larkws.EndpointResp
	accepted chan *websocket.Conn
}

func newWSGateway(t *testing.T) *wsGateway {
	t.Helper()
	g := &wsGateway{
		fetching: make(chan struct{}),
		script:   make(chan larkws.EndpointResp),
		accepted: make(chan *websocket.Conn, 4),
	}
	stop := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc(larkws.GenEndpointUri, func(w http.ResponseWriter, r *http.Request) {
		var resp larkws.EndpointResp
		select {
		case g.fetching <- struct{}{}:
		case <-stop:
			return
		}
		select {
		case resp = <-g.script:
		case <-stop:
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&resp)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		g.accepted <- conn
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		close(stop)
		g.srv.CloseClientConnections()
		g.srv.Close()
	})
	return g
}

func (g *wsGateway) ok() larkws.EndpointResp {
	return larkws.EndpointResp{Code: larkws.OK, Data: &larkws.Endpoint{
		Url: "ws" + strings.TrimPrefix(g.srv.URL, "http") + "/ws",
		ClientConfig: &larkws.ClientConfig{
			ReconnectCount: 5, ReconnectInterval: 0, ReconnectNonce: 0, PingInterval: 3600,
		},
	}}
}

func (g *wsGateway) awaitFetch(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.fetching:
	case <-time.After(connStateTestTimeout):
		t.Fatalf("SDK never fetched the endpoint (%s)", what)
	}
}

func (g *wsGateway) reply(t *testing.T, resp larkws.EndpointResp) {
	t.Helper()
	select {
	case g.script <- resp:
	case <-time.After(connStateTestTimeout):
		t.Fatal("endpoint handler never took its reply")
	}
}

func (g *wsGateway) awaitConn(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-g.accepted:
		return c
	case <-time.After(connStateTestTimeout):
		t.Fatal("SDK never dialled the websocket")
		return nil
	}
}

func mustConnState(t *testing.T, f *Feishu) platform.ConnState {
	t.Helper()
	s, ok := f.ConnState()
	if !ok {
		t.Fatal("websocket mode reported not observable after start")
	}
	return s
}

func awaitConnState(t *testing.T, f *Feishu, want platform.ConnStateKind) platform.ConnState {
	t.Helper()
	testhelper.Eventually(t, func() bool {
		s, ok := f.ConnState()
		return ok && s.State == want
	}, connStateTestTimeout, "feishu conn state should become "+string(want))
	return mustConnState(t, f)
}

// TestWebSocket_ConnStateFollowsLarkwsLifecycle drives the real larkws client
// against a fake gateway through first connect, a dropped link, a retryable
// lookup failure and a non-retryable one, checking the state at each step.
func TestWebSocket_ConnStateFollowsLarkwsLifecycle(t *testing.T) {
	t.Parallel()
	g := newWSGateway(t)
	f := &Feishu{
		mode:     "websocket",
		baseURL:  g.srv.URL,
		cfg:      Config{AppID: "cli_test", AppSecret: "secret"},
		dispatch: platform.BoundedDispatch{Name: "feishu"},
		handler:  func(context.Context, platform.IncomingMessage) {},
	}
	f.stopCtx, f.stopCancel = context.WithCancel(context.Background())
	t.Cleanup(func() { _ = f.Stop() })

	if err := f.startWebSocket(); err != nil {
		t.Fatalf("startWebSocket: %v", err)
	}

	// The SDK is parked in its first endpoint lookup: nothing has connected.
	g.awaitFetch(t, "initial")
	if s := mustConnState(t, f); s.State != platform.ConnConnecting {
		t.Fatalf("before the first connect: state %q, want connecting", s.State)
	}
	g.reply(t, g.ok())
	conn1 := g.awaitConn(t)
	awaitConnState(t, f, platform.ConnConnected)

	// Dropping the link sends the SDK back to the endpoint lookup, by which
	// time OnDisconnected and OnReconnecting have both fired.
	_ = conn1.Close()
	g.awaitFetch(t, "after the drop")
	s := mustConnState(t, f)
	if s.State != platform.ConnConnecting {
		t.Fatalf("while reconnecting: state %q, want connecting", s.State)
	}
	g.reply(t, g.ok())
	conn2 := g.awaitConn(t)
	awaitConnState(t, f, platform.ConnConnected)

	// A retryable lookup failure is recorded but leaves the state alone; the
	// SDK is already in its next attempt when we look.
	_ = conn2.Close()
	g.awaitFetch(t, "second drop")
	g.reply(t, larkws.EndpointResp{Code: larkws.SystemBusy, Msg: "system busy"})
	g.awaitFetch(t, "retry after busy")
	s = mustConnState(t, f)
	if s.State != platform.ConnConnecting {
		t.Fatalf("after a retryable failure: state %q, want connecting", s.State)
	}
	if !strings.Contains(s.LastError, "system busy") || s.LastErrorAt.IsZero() {
		t.Fatalf("retryable failure not recorded: %+v", s)
	}

	// A client error (bad credentials) is terminal: Start returns and the
	// adapter reports failed with the SDK's reason.
	g.reply(t, larkws.EndpointResp{Code: 10003, Msg: "app secret invalid"})
	s = awaitConnState(t, f, platform.ConnFailed)
	if !strings.Contains(s.LastError, "app secret invalid") {
		t.Fatalf("terminal failure: LastError %q, want the SDK reason", s.LastError)
	}
	select {
	case <-f.done:
	case <-time.After(connStateTestTimeout):
		t.Fatal("websocket goroutine still running after a terminal failure")
	}
}

// TestWebSocket_StopIsNotAFailure: cancelling the transport makes Start
// return with ctx done, which is a shutdown, not the SDK giving up; the link
// it closed reads as disconnected rather than still connected.
func TestWebSocket_StopIsNotAFailure(t *testing.T) {
	t.Parallel()
	g := newWSGateway(t)
	f := &Feishu{
		mode:     "websocket",
		baseURL:  g.srv.URL,
		cfg:      Config{AppID: "cli_test", AppSecret: "secret"},
		dispatch: platform.BoundedDispatch{Name: "feishu"},
		handler:  func(context.Context, platform.IncomingMessage) {},
	}
	f.stopCtx, f.stopCancel = context.WithCancel(context.Background())

	if err := f.startWebSocket(); err != nil {
		t.Fatalf("startWebSocket: %v", err)
	}
	g.awaitFetch(t, "initial")
	g.reply(t, g.ok())
	g.awaitConn(t)
	awaitConnState(t, f, platform.ConnConnected)

	if err := f.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	s := mustConnState(t, f)
	if s.State != platform.ConnDisconnected || s.LastError != "" {
		t.Fatalf("after Stop: %+v, want a plain disconnect", s)
	}
}
