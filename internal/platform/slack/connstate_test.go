package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const connStateTestTimeout = 5 * time.Second

func TestConnState_BeforeStartIsNotObservable(t *testing.T) {
	t.Parallel()
	s := New(Config{BotToken: "xoxb-test", AppToken: "xapp-test"})
	if st, ok := s.ConnState(); ok {
		t.Fatalf("unstarted adapter reported %+v, want not observable", st)
	}
}

// TestHandleSocketEvent_ConnState pins how each socket mode lifecycle event
// moves the tracked state. A nil client is fine: only events_api acks.
func TestHandleSocketEvent_ConnState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prior   platform.ConnStateKind
		evt     socketmode.Event
		want    platform.ConnStateKind
		wantErr string // substring of LastError; "" means none recorded
	}{
		{"connecting", platform.ConnConnected,
			socketmode.Event{Type: socketmode.EventTypeConnecting, Data: &slack.ConnectingEvent{Attempt: 1}},
			platform.ConnConnecting, ""},
		{"connected", platform.ConnConnecting,
			socketmode.Event{Type: socketmode.EventTypeConnected, Data: &socketmode.ConnectedEvent{}},
			platform.ConnConnected, ""},
		{"hello", platform.ConnConnecting,
			socketmode.Event{Type: socketmode.EventTypeHello},
			platform.ConnConnected, ""},
		{"connection error retries", platform.ConnConnecting,
			socketmode.Event{Type: socketmode.EventTypeConnectionError,
				Data: &slack.ConnectionErrorEvent{Attempt: 1, ErrorObj: errors.New("dial tcp: i/o timeout")}},
			platform.ConnConnecting, "i/o timeout"},
		{"connection error without an error", platform.ConnConnecting,
			socketmode.Event{Type: socketmode.EventTypeConnectionError, Data: &slack.ConnectionErrorEvent{}},
			platform.ConnConnecting, ""},
		{"incoming error keeps the state", platform.ConnConnected,
			socketmode.Event{Type: socketmode.EventTypeIncomingError,
				Data: &slack.IncomingEventError{ErrorObj: errors.New("websocket: close 1006")}},
			platform.ConnConnected, "close 1006"},
		{"invalid auth is terminal", platform.ConnConnecting,
			socketmode.Event{Type: socketmode.EventTypeInvalidAuth, Data: &slack.InvalidAuthEvent{}},
			platform.ConnFailed, "invalid_auth"},
		{"unrelated event", platform.ConnConnected,
			socketmode.Event{Type: socketmode.EventTypeErrorBadMessage},
			platform.ConnConnected, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Slack{}
			s.connState.Set(tc.prior)
			s.handleSocketEvent(context.Background(), nil, tc.evt)
			st, ok := s.ConnState()
			if !ok || st.State != tc.want {
				t.Fatalf("state %+v (ok=%v), want %q", st, ok, tc.want)
			}
			if tc.wantErr == "" {
				if st.LastError != "" {
					t.Fatalf("LastError %q, want none", st.LastError)
				}
			} else if !strings.Contains(st.LastError, tc.wantErr) || st.LastErrorAt.IsZero() {
				t.Fatalf("LastError %q at %v, want it to contain %q", st.LastError, st.LastErrorAt, tc.wantErr)
			}
		})
	}
}

// TestEventLoop_GiveUpLandsAfterBufferedEvents: when RunContext returns, the
// events it buffered before giving up are handled first, so the last state
// is failed, with the reason redacted like any other. Both channels are ready, so select picks either; repeating makes
// a missing drain fail with near certainty.
func TestEventLoop_GiveUpLandsAfterBufferedEvents(t *testing.T) {
	t.Parallel()
	for i := 0; i < 32; i++ {
		s := &Slack{}
		client := socketmode.New(slack.New("xoxb-test"))
		client.Events <- socketmode.Event{Type: socketmode.EventTypeConnecting, Data: &slack.ConnectingEvent{Attempt: 1}}
		runErr := make(chan error, 1)
		runErr <- &url.Error{Op: "Post", URL: "wss://wss.slack.test/link/?ticket=SECRET", Err: errors.New("invalid_auth")}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.eventLoop(ctx, client, runErr)
		}()
		testhelper.Eventually(t, func() bool {
			st, _ := s.ConnState()
			return st.State == platform.ConnFailed && len(client.Events) == 0
		}, connStateTestTimeout, "event loop should record the give-up")
		cancel()
		<-done
		st, _ := s.ConnState()
		if st.State != platform.ConnFailed || !strings.Contains(st.LastError, "invalid_auth") || strings.Contains(st.LastError, "SECRET") {
			t.Fatalf("iteration %d: final state %+v, want failed with a redacted reason", i, st)
		}
	}
}

// TestEventLoop_ShutdownIsNotAFailure: RunContext returning because ctx
// ended is a shutdown even when the loop takes runErr before ctx.Done.
func TestEventLoop_ShutdownIsNotAFailure(t *testing.T) {
	t.Parallel()
	for i := 0; i < 32; i++ {
		s := &Slack{}
		s.connState.Set(platform.ConnConnected)
		client := socketmode.New(slack.New("xoxb-test"))
		runErr := make(chan error, 1)
		runErr <- context.Canceled
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.eventLoop(ctx, client, runErr)
		if st, _ := s.ConnState(); st.State != platform.ConnConnected || st.LastError != "" {
			t.Fatalf("iteration %d: %+v after a shutdown, want connected and no error", i, st)
		}
	}
}

func TestRedactURLQuery(t *testing.T) {
	t.Parallel()
	// What url.Parse returns for these URLs.
	parseErr := &url.Error{Op: "parse", URL: "wss://wss.slack.test/link/%zz?ticket=SECRET&app_id=A1", Err: url.EscapeError("%zz")}
	// %q escapes the quote, so the error text differs from the raw URL.
	quotedErr := &url.Error{Op: "parse", URL: `wss://wss.slack.test/%zz?ticket=SE"CRET`, Err: url.EscapeError("%zz")}
	plain := errors.New("invalid_auth")
	noQuery := &url.Error{Op: "Post", URL: "https://slack.com/api/apps.connections.open", Err: errors.New("EOF")}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"parse error", parseErr,
			`parse "wss://wss.slack.test/link/%zz?<redacted>": invalid URL escape "%zz"`},
		{"wrapped", fmt.Errorf("dial: %w", parseErr),
			`dial: parse "wss://wss.slack.test/link/%zz?<redacted>": invalid URL escape "%zz"`},
		{"escaped in the message", quotedErr,
			`parse "wss://wss.slack.test/%zz?<redacted>": invalid URL escape "%zz"`},
		{"no query", noQuery, noQuery.Error()},
		{"not a URL error", plain, "invalid_auth"},
	}
	for _, tc := range cases {
		got := redactURLQuery(tc.err)
		if got == nil || got.Error() != tc.want {
			t.Errorf("%s: got %v, want %q", tc.name, got, tc.want)
		}
	}
	if redactURLQuery(nil) != nil {
		t.Error("nil error must stay nil")
	}
}

// fakeSlack serves the two Web API methods Start needs and the socket mode
// websocket. auth.test and apps.connections.open each wait for the test, so
// it can look at the adapter while Start or the client is parked in a call.
type fakeSlack struct {
	srv      *httptest.Server
	authing  chan struct{} // one send per auth.test, before it replies
	authGate chan struct{}
	opening  chan struct{} // one send per apps.connections.open, before it replies
	script   chan map[string]any
	accepted chan *websocket.Conn
	inbound  chan []byte // frames the client wrote; dropped when full
	mux      *http.ServeMux
	handler  platform.MessageHandler // Start's handler; nil = discard
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{
		authing:  make(chan struct{}),
		authGate: make(chan struct{}),
		opening:  make(chan struct{}),
		script:   make(chan map[string]any),
		accepted: make(chan *websocket.Conn, 4),
		inbound:  make(chan []byte, 16),
	}
	stop := make(chan struct{})
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, r *http.Request) {
		select {
		case f.authing <- struct{}{}:
		case <-stop:
			return
		}
		select {
		case <-f.authGate:
		case <-stop:
			return
		}
		writeJSON(w, map[string]any{"ok": true, "user_id": "UBOT", "team": "T"})
	})
	mux.HandleFunc("/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		var resp map[string]any
		select {
		case f.opening <- struct{}{}:
		case <-stop:
			return
		}
		select {
		case resp = <-f.script:
		case <-stop:
			return
		}
		writeJSON(w, resp)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// hello goes first so a test may write to conn once it is accepted.
		_ = conn.WriteJSON(map[string]any{"type": "hello", "num_connections": 1})
		f.accepted <- conn
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case f.inbound <- data:
			default:
			}
		}
	})
	f.mux = mux
	f.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		close(stop)
		f.srv.CloseClientConnections()
		f.srv.Close()
	})
	return f
}

// adapter returns a Slack whose Web API calls go to the fake.
func (f *fakeSlack) adapter() *Slack {
	cfg := Config{BotToken: "xoxb-test", AppToken: "xapp-test"}
	s := New(cfg)
	s.api = slack.New(cfg.BotToken,
		slack.OptionAppLevelToken(cfg.AppToken),
		slack.OptionHTTPClient(slackHTTPClient),
		slack.OptionAPIURL(f.srv.URL+"/"),
	)
	return s
}

// start runs s.Start, checking the state while Start is still in its
// AuthTest: the adapter already reports connecting, not "registered".
func (f *fakeSlack) start(t *testing.T, s *Slack) {
	t.Helper()
	errc := make(chan error, 1)
	handler := f.handler
	if handler == nil {
		handler = func(context.Context, platform.IncomingMessage) {}
	}
	go func() { errc <- s.Start(handler) }()
	select {
	case <-f.authing:
	case <-time.After(connStateTestTimeout):
		t.Fatal("Start never called auth.test")
	}
	if st, ok := s.ConnState(); !ok || st.State != platform.ConnConnecting {
		t.Errorf("during AuthTest: %+v (ok=%v), want connecting", st, ok)
	}
	close(f.authGate)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(connStateTestTimeout):
		t.Fatal("Start did not return")
	}
}

func (f *fakeSlack) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws"
}

func (f *fakeSlack) awaitOpen(t *testing.T, what string) {
	t.Helper()
	select {
	case <-f.opening:
	case <-time.After(connStateTestTimeout):
		t.Fatalf("client never called apps.connections.open (%s)", what)
	}
}

func (f *fakeSlack) reply(t *testing.T, resp map[string]any) {
	t.Helper()
	select {
	case f.script <- resp:
	case <-time.After(connStateTestTimeout):
		t.Fatal("apps.connections.open handler never took its reply")
	}
}

func (f *fakeSlack) awaitConn(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-f.accepted:
		return c
	case <-time.After(connStateTestTimeout):
		t.Fatal("client never dialled the websocket")
		return nil
	}
}

func awaitConnState(t *testing.T, s *Slack, what string, cond func(platform.ConnState) bool) platform.ConnState {
	t.Helper()
	testhelper.Eventually(t, func() bool {
		st, ok := s.ConnState()
		return ok && cond(st)
	}, connStateTestTimeout, "slack conn state: "+what)
	st, _ := s.ConnState()
	return st
}

func stateIs(want platform.ConnStateKind) func(platform.ConnState) bool {
	return func(st platform.ConnState) bool { return st.State == want }
}

// TestSocketMode_ConnStateFollowsClientLifecycle drives the real socket mode
// client through first connect, a dropped link, a retryable connect failure
// whose error quotes the ticketed URL, and a terminal invalid_auth.
func TestSocketMode_ConnStateFollowsClientLifecycle(t *testing.T) {
	t.Parallel()
	f := newFakeSlack(t)
	s := f.adapter()
	t.Cleanup(func() { _ = s.Stop() })
	f.start(t, s)

	f.awaitOpen(t, "initial")
	awaitConnState(t, s, "connecting before the first connect", stateIs(platform.ConnConnecting))
	f.reply(t, map[string]any{"ok": true, "url": f.wsURL()})
	conn := f.awaitConn(t)
	awaitConnState(t, s, "connected", stateIs(platform.ConnConnected))

	// A dropped link sends the client back to apps.connections.open.
	_ = conn.Close()
	f.awaitOpen(t, "after the drop")
	outage := awaitConnState(t, s, "connecting after the drop", stateIs(platform.ConnConnecting))

	// A failed attempt is recorded, the client retries after its backoff,
	// and the outage keeps its original Since. The ticket never surfaces.
	f.reply(t, map[string]any{"ok": true, "url": "ws://127.0.0.1/%zz?ticket=SECRET-TICKET"})
	f.awaitOpen(t, "retry after a connect failure")
	st := awaitConnState(t, s, "connect failure recorded", func(st platform.ConnState) bool {
		return st.LastError != ""
	})
	if st.State != platform.ConnConnecting || !st.Since.Equal(outage.Since) {
		t.Fatalf("after a retryable failure: %+v, want connecting since %v", st, outage.Since)
	}
	if !strings.Contains(st.LastError, "invalid URL escape") || strings.Contains(st.LastError, "SECRET") {
		t.Fatalf("LastError %q: want the parse failure without the ticket", st.LastError)
	}

	// Bad credentials are terminal: RunContext returns and the adapter
	// reports failed with the reason.
	f.reply(t, map[string]any{"ok": false, "error": "invalid_auth"})
	awaitConnState(t, s, "failed", stateIs(platform.ConnFailed))
	// Stop waits out the event loop, so any event still buffered when
	// RunContext gave up has been handled by now: failed must survive it.
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, _ = s.ConnState()
	if st.State != platform.ConnFailed || !strings.Contains(st.LastError, "invalid_auth") {
		t.Fatalf("terminal failure: %+v, want failed with invalid_auth", st)
	}
}

// TestSocketMode_StopIsNotAFailure: Stop cancels RunContext, which returns
// with ctx done; that is a shutdown, not the client giving up.
func TestSocketMode_StopIsNotAFailure(t *testing.T) {
	t.Parallel()
	f := newFakeSlack(t)
	s := f.adapter()
	f.start(t, s)
	f.awaitOpen(t, "initial")
	f.reply(t, map[string]any{"ok": true, "url": f.wsURL()})
	f.awaitConn(t)
	awaitConnState(t, s, "connected", stateIs(platform.ConnConnected))

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, _ := s.ConnState()
	if st.State == platform.ConnFailed || st.LastError != "" {
		t.Fatalf("after Stop: %+v, want no failure recorded", st)
	}
}
