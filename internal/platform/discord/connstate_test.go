package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const connStateTestTimeout = 10 * time.Second

// restBotID is the fake's answer to GET /users/@me; READY carries "bot-1".
const restBotID = "bot-rest"

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

// fakeGateway serves Discord's REST API (every path answers like the gateway
// lookup) and a websocket gateway that answers Hello / Identify / Resume. Each websocket handshake waits for
// one value on hello, so a test can observe the state while Open is blocked.
// A connection is published on conns only after the client's first
// heartbeat, so closing it cannot fail that heartbeat and start a second
// reconnect. With holdUser set, GET /users/@me signals userHeld and answers
// only once userRelease is closed; otherwise it answers as user restBotID.
// Interaction responses are answered 204 and their bodies sent on callbacks.
type fakeGateway struct {
	srv         *httptest.Server
	restStatus  atomic.Int32 // non-zero: every REST request fails with this status
	userCalls   atomic.Int32 // GET /users/@me requests, i.e. disconnect probes
	holdUser    atomic.Bool
	userHeld    chan struct{}
	userRelease chan struct{}
	dials       atomic.Int32
	hello       chan struct{}
	conns       chan *websocket.Conn
	callbacks   chan []byte
	done        chan struct{}
}

func newFakeGateway(t *testing.T, restStatus int) *fakeGateway {
	t.Helper()
	g := &fakeGateway{
		userHeld:    make(chan struct{}, 1),
		userRelease: make(chan struct{}),
		hello:       make(chan struct{}, 4),
		conns:       make(chan *websocket.Conn, 4),
		callbacks:   make(chan []byte, 4),
		done:        make(chan struct{}),
	}
	g.restStatus.Store(int32(restStatus))
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	t.Cleanup(func() { close(g.done) })
	return g
}

func (g *fakeGateway) serve(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		if strings.Contains(r.URL.Path, "/interactions/") {
			body, _ := io.ReadAll(r.Body)
			select {
			case g.callbacks <- body:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/users/@me") {
			g.userCalls.Add(1)
			if g.holdUser.Load() {
				select {
				case g.userHeld <- struct{}{}:
				default:
				}
				select {
				case <-g.userRelease:
				case <-g.done:
					return
				}
			}
		}
		if status := int(g.restStatus.Load()); status != 0 {
			http.Error(w, `{"message": "401: Unauthorized", "code": 0}`, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/users/@me") {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": restBotID, "username": "naozhi"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws://" + r.Host})
		return
	}
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	g.dials.Add(1)
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
	for {
		var msg struct {
			Op int `json:"op"`
		}
		if conn.ReadJSON(&msg) != nil {
			return
		}
		if msg.Op == 1 { // Heartbeat
			break
		}
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
	if n := g.dials.Load(); n != 2 {
		t.Fatalf("gateway saw %d connections, want 2 (one drop, one reconnect)", n)
	}
}

// TestStop_BoundedWhileHandshakeHoldsLock: discordgo's Open holds the
// session lock until Hello arrives, with no read deadline, so a gateway that
// stalls a reconnect handshake leaves Close waiting on that lock. Holding the
// lock here stands in for the stalled Open; Stop must still return.
func TestStop_BoundedWhileHandshakeHoldsLock(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t, 0)
	d := newGatewayAdapter(t, g)
	d.closeTimeout = 100 * time.Millisecond
	g.hello <- struct{}{}
	if err := d.Start(func(context.Context, platform.IncomingMessage) {}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-g.conns
	closed := make(chan struct{})
	var closeOnce sync.Once
	d.session.AddHandler(func(*discordgo.Session, *discordgo.Disconnect) {
		closeOnce.Do(func() { close(closed) })
	})

	d.session.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(d.session.Unlock) }
	t.Cleanup(release)

	stopped := make(chan error, 1)
	go func() { stopped <- d.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(connStateTestTimeout):
		t.Fatal("Stop blocked behind the held session lock")
	}

	// The abandoned Close completes once the lock is free.
	release()
	select {
	case <-closed:
	case <-time.After(connStateTestTimeout):
		t.Fatal("abandoned Close never finished")
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

// startDropped starts d against g, then drops the link with REST answering
// status, leaving the reconnect parked on its handshake until the test feeds
// g.hello.
func startDropped(t *testing.T, g *fakeGateway, d *Discord, status int) {
	t.Helper()
	d.probeDelay = 10 * time.Millisecond
	d.probeMaxInterval = 20 * time.Millisecond
	g.hello <- struct{}{}
	if err := d.Start(func(context.Context, platform.IncomingMessage) {}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	first := <-g.conns
	waitConnState(t, d, platform.ConnConnected)
	g.restStatus.Store(int32(status))
	_ = first.Close()
}

// reconnect lets the parked reconnect finish and waits for connected.
func reconnect(t *testing.T, g *fakeGateway, d *Discord) platform.ConnState {
	t.Helper()
	g.restStatus.Store(0)
	g.hello <- struct{}{}
	<-g.conns
	return waitConnState(t, d, platform.ConnConnected)
}

// TestConnState_DropWithRevokedTokenFails: a token revoked mid-run makes every
// reconnect fail without an event, so only the REST probe can tell the
// operator; the state goes terminal and names the fix without leaking the
// token or the response body.
func TestConnState_DropWithRevokedTokenFails(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			g := newFakeGateway(t, 0)
			d := newGatewayAdapter(t, g)
			startDropped(t, g, d, status)

			st := waitConnState(t, d, platform.ConnFailed)
			// Full text pinned: doctor says the same thing (#3514), and discordgo
			// keeps reconnecting, so a restart is not always needed.
			want := "discord rejected the bot token (HTTP " + strconv.Itoa(status) + "): update platforms.discord.bot_token (restart unless it reconnects by itself)"
			if st.LastError != want || st.LastErrorAt.IsZero() {
				t.Fatalf("LastError = %q at %v, want %q", st.LastError, st.LastErrorAt, want)
			}
			if strings.Contains(st.LastError, "test-token") || strings.Contains(st.LastError, "message") {
				t.Fatalf("LastError leaks the token or the response body: %q", st.LastError)
			}

			back := reconnect(t, g, d)
			if back.LastError != st.LastError {
				t.Fatalf("after reconnect LastError = %q, want the probe's %q kept", back.LastError, st.LastError)
			}
		})
	}
}

// TestConnState_DropNotesProbeError: a REST failure that says nothing about
// the token is recorded as the reason without leaving disconnected, and the
// probe keeps checking until the gateway is back, then stops. Only the status
// is kept: 502 (after discordgo's own retries) and 429 do not arrive as a
// RESTError, and the fake's body must not reach LastError on any of them.
func TestConnState_DropNotesProbeError(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			g := newFakeGateway(t, 0)
			d := newGatewayAdapter(t, g)
			startDropped(t, g, d, status)

			want := "gateway down; REST probe: HTTP " + strconv.Itoa(status)
			var st platform.ConnState
			testhelper.Eventually(t, func() bool {
				st, _ = d.ConnState()
				return st.LastError != ""
			}, connStateTestTimeout, "the probe noted nothing")
			if st.State != platform.ConnDisconnected || st.LastError != want {
				t.Fatalf("state = %+v, want disconnected with LastError %q", st, want)
			}
			calls := g.userCalls.Load()
			testhelper.Eventually(t, func() bool { return g.userCalls.Load() > calls },
				connStateTestTimeout, "the probe stopped after its first failed check")

			back := reconnect(t, g, d)
			if back.LastError != st.LastError {
				t.Fatalf("after reconnect LastError = %q, want %q kept", back.LastError, st.LastError)
			}
			testhelper.Eventually(t, func() bool { return !d.probing.Load() },
				connStateTestTimeout, "the probe kept running after the gateway reconnected")
		})
	}
}

// TestConnState_LateProbeVerdictKeepsReconnect: a probe answer that arrives
// after the gateway came back neither fails the live link nor leaves a
// "gateway down" reason behind.
func TestConnState_LateProbeVerdictKeepsReconnect(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			g := newFakeGateway(t, 0)
			d := newGatewayAdapter(t, g)
			g.holdUser.Store(true)
			startDropped(t, g, d, status)

			<-g.userHeld
			g.hello <- struct{}{}
			<-g.conns
			waitConnState(t, d, platform.ConnConnected)
			close(g.userRelease)
			testhelper.Eventually(t, func() bool { return !d.probing.Load() },
				connStateTestTimeout, "the probe kept running after the gateway reconnected")

			st, _ := d.ConnState()
			if st.State != platform.ConnConnected || st.LastError != "" {
				t.Fatalf("after a late HTTP %d verdict state = %+v, want connected with no LastError", status, st)
			}
		})
	}
}

// TestOnDisconnect_ProbeLifetime: a Disconnect during Stop starts no probe;
// a drop starts one, and Stop ends it even mid-wait.
func TestOnDisconnect_ProbeLifetime(t *testing.T) {
	t.Parallel()
	sess, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}

	stopped := New(Config{BotToken: "test-token"})
	ctx, cancel := context.WithCancel(context.Background())
	stopped.stopCtx = ctx
	cancel()
	stopped.onDisconnect(sess, &discordgo.Disconnect{})
	if stopped.probing.Load() {
		t.Fatal("a Disconnect after Stop started a probe")
	}

	d := New(Config{BotToken: "test-token"})
	ctx, cancel = context.WithCancel(context.Background())
	d.stopCtx = ctx
	d.probeDelay = time.Hour
	d.onDisconnect(sess, &discordgo.Disconnect{})
	if !d.probing.Load() {
		t.Fatal("a drop started no probe")
	}
	cancel()
	done := make(chan struct{})
	go func() { d.dispatch.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(connStateTestTimeout):
		t.Fatal("the probe outlived Stop")
	}
	if d.probing.Load() {
		t.Fatal("probing still set after the probe exited")
	}
}

// startWithoutBotID starts d against g and forgets the identity READY gave
// it, as when Open returns without a READY frame.
func startWithoutBotID(t *testing.T, g *fakeGateway, d *Discord) {
	t.Helper()
	g.hello <- struct{}{}
	if err := d.Start(func(context.Context, platform.IncomingMessage) {}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	<-g.conns
	waitConnState(t, d, platform.ConnConnected)
	d.botID.Store(nil)
}

// waitDispatch waits for the adapter's background goroutines.
func waitDispatch(t *testing.T, d *Discord, msg string) {
	t.Helper()
	done := make(chan struct{})
	go func() { d.dispatch.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(connStateTestTimeout):
		t.Fatal(msg)
	}
}

func TestBotHeal_RecoversBotID(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t, 0)
	d := newGatewayAdapter(t, g)
	startWithoutBotID(t, g, d)

	d.maybeHealBotID()
	waitDispatch(t, d, "the self-heal never finished")
	if got := d.getBotID(); got != restBotID {
		t.Fatalf("botID = %q, want %q from REST", got, restBotID)
	}
}

// TestBotHeal_StopDoesNotWaitOnHeldRequest: Stop cancels a self-heal whose
// request the server never answers instead of waiting out the HTTP client
// timeout (20s).
func TestBotHeal_StopDoesNotWaitOnHeldRequest(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t, 0)
	d := newGatewayAdapter(t, g)
	d.probeTimeout = time.Hour
	startWithoutBotID(t, g, d)
	g.holdUser.Store(true)

	d.maybeHealBotID()
	<-g.userHeld
	stopped := make(chan struct{})
	go func() { _ = d.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop waited on the held self-heal request")
	}
}

// TestBotHeal_BoundedByProbeTimeout: an unanswered self-heal gives up after
// probeTimeout rather than the HTTP client timeout.
func TestBotHeal_BoundedByProbeTimeout(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t, 0)
	d := newGatewayAdapter(t, g)
	d.probeTimeout = 50 * time.Millisecond
	startWithoutBotID(t, g, d)
	g.holdUser.Store(true)

	d.maybeHealBotID()
	<-g.userHeld
	start := time.Now()
	waitDispatch(t, d, "the held self-heal request never timed out")
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("self-heal gave up after %v, want about probeTimeout", took)
	}
	if got := d.getBotID(); got != "" {
		t.Fatalf("botID = %q after a timed-out heal, want empty", got)
	}
}

// TestBotHeal_RateLimitedIsNotRetried: the fake's 429 carries no retry_after,
// so discordgo's own retry would re-request in a loop; the heal makes one
// request and waits for the cooldown instead.
func TestBotHeal_RateLimitedIsNotRetried(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t, 0)
	d := newGatewayAdapter(t, g)
	startWithoutBotID(t, g, d)
	g.restStatus.Store(http.StatusTooManyRequests)

	d.maybeHealBotID()
	waitDispatch(t, d, "the rate-limited self-heal kept retrying")
	if n := g.userCalls.Load(); n != 1 {
		t.Fatalf("GET /users/@me made %d times, want 1", n)
	}
	if got := d.getBotID(); got != "" {
		t.Fatalf("botID = %q after a 429, want empty", got)
	}
}

// TestBotHeal_FailureLogsStatusNotBody: a failed heal logs the HTTP status,
// not the response body, which can be an HTML page. Not parallel: it swaps
// slog's default handler.
func TestBotHeal_FailureLogsStatusNotBody(t *testing.T) {
	var mu sync.Mutex
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	g := newFakeGateway(t, 0)
	d := newGatewayAdapter(t, g)
	startWithoutBotID(t, g, d)
	g.restStatus.Store(http.StatusUnauthorized)

	d.maybeHealBotID()
	waitDispatch(t, d, "the failing self-heal never finished")
	mu.Lock()
	got := logs.String()
	mu.Unlock()
	if !strings.Contains(got, "self-heal failed") || !strings.Contains(got, "HTTP 401") {
		t.Fatalf("heal failure log lacks the status:\n%s", got)
	}
	if strings.Contains(got, "Unauthorized") {
		t.Fatalf("heal failure log carries the response body:\n%s", got)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestNextProbeDelay_DoublesToTheCap(t *testing.T) {
	t.Parallel()
	got := []time.Duration{discordProbeDelay}
	for len(got) < 9 {
		got = append(got, nextProbeDelay(got[len(got)-1], discordProbeMaxInterval))
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	if !slices.Equal(got, want) {
		t.Fatalf("probe delays = %v, want %v", got, want)
	}
}
