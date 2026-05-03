package upstream

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// handleConnDrainBudget is the hard deadline applied to the deferred
// wg.Wait() at the end of handleConn. Every worker goroutine is expected
// to honour connCtx, which is cancelled the moment handleConn returns; the
// budget covers the pathological case where a stuck downstream call (most
// notably sess.Send blocked on CLI watchdog timeout ≈ 5 min) refuses to
// unblock. Hit-budget leaks the stuck goroutine to process teardown —
// strictly better than pinning the whole upstream reconnect loop on one
// slow session. R51-REL-005. Package-level var (not const) so tests can
// shorten it without wall-clock waits.
var handleConnDrainBudget = 15 * time.Second

// Connector dials a primary naozhi and serves it as a reverse-connected node.
// Run on machines behind NAT that cannot be reached by the primary directly.
type Connector struct {
	cfg              *config.UpstreamConfig
	router           *session.Router
	projMgr          *project.Manager // may be nil
	claudeDir        string
	hostname         string
	defaultWorkspace string // used as allowedRoot for incoming workspace overrides
	discoverFunc     func() (json.RawMessage, error)
	previewFunc      func(sessionID string) (json.RawMessage, error)
}

// New creates a Connector. projMgr may be nil if projects are not configured.
func New(cfg *config.UpstreamConfig, router *session.Router, projMgr *project.Manager) *Connector {
	claudeDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		claudeDir = filepath.Join(home, ".claude")
	}
	hostname, _ := os.Hostname()
	return &Connector{
		cfg:              cfg,
		router:           router,
		projMgr:          projMgr,
		claudeDir:        claudeDir,
		hostname:         hostname,
		defaultWorkspace: router.DefaultWorkspace(),
	}
}

// SetDiscoverFunc sets a callback that returns discovered sessions as JSON.
func (c *Connector) SetDiscoverFunc(fn func() (json.RawMessage, error)) {
	c.discoverFunc = fn
}

// SetPreviewFunc sets a callback that returns conversation history for a discovered session.
func (c *Connector) SetPreviewFunc(fn func(sessionID string) (json.RawMessage, error)) {
	c.previewFunc = fn
}

// Run connects to the primary and serves requests. Reconnects on disconnect.
// Blocks until ctx is cancelled.
func (c *Connector) Run(ctx context.Context) {
	backoff := time.Second
	for {
		connected, err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("connector disconnected", "url", c.cfg.URL, "err", err)
		}
		// Reset backoff after a successful session so reconnect after
		// sleep/restart is fast (1s) rather than up to 30s.
		if connected {
			backoff = time.Second
		}
		// Jitter the sleep so many connectors restarted together (e.g. fleet
		// SIGHUP) don't hammer the primary on aligned deadlines. backoff
		// still doubles deterministically; we only scatter wall-time.
		timer := time.NewTimer(jitterBackoff(backoff))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

func (c *Connector) runOnce(ctx context.Context) (bool, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		// Pin TLS floor so downgraded clients can't be forced onto a weaker
		// protocol via a compromised network segment. wss:// is already
		// required by config validation.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	conn, _, dialErr := dialer.DialContext(ctx, c.cfg.URL, nil)
	if dialErr != nil {
		return false, fmt.Errorf("dial: %w", dialErr)
	}
	// Bound inbound frame size so a malicious or buggy primary cannot
	// exhaust memory with a single huge message. 16 MB matches the primary
	// side's ReverseConn limit (reverseserver.go).
	conn.SetReadLimit(16 << 20)

	// gorilla/websocket's Conn.Close is documented for one concurrent
	// reader and one concurrent writer but not for concurrent Close calls.
	// The cancel-watchdog goroutine below calls conn.Close on ctx.Done, and
	// the deferred close on function exit would race with it. Serialize
	// both paths through a sync.Once so exactly one Close ever fires.
	// R60-GO-M5.
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }
	defer closeConn()

	// Close the WebSocket when ctx is cancelled to unblock ReadJSON in handleConn.
	connDone := make(chan struct{})
	defer close(connDone)
	go func() {
		select {
		case <-ctx.Done():
			closeConn()
		case <-connDone:
		}
	}()

	// Register
	reg := node.ReverseMsg{
		Type:        "register",
		NodeID:      c.cfg.NodeID,
		Token:       c.cfg.Token,
		DisplayName: c.cfg.DisplayName,
		Hostname:    c.hostname,
	}
	if err := conn.WriteJSON(reg); err != nil {
		return false, fmt.Errorf("register write: %w", err)
	}

	// SetReadDeadline error means the underlying net.Conn is already torn
	// down — returning early is correct because ReadJSON below would block
	// forever without a deadline. The same applies to the clear below and
	// the pong-path deadlines downstream.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return false, fmt.Errorf("set register read deadline: %w", err)
	}
	var ack node.ReverseMsg
	if err := conn.ReadJSON(&ack); err != nil {
		return false, fmt.Errorf("register ack read: %w", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return false, fmt.Errorf("clear register read deadline: %w", err)
	}

	if ack.Type != "registered" {
		// %q so primary-controlled Error string can't inject key=val pairs or
		// newlines into slog output downstream.
		return false, fmt.Errorf("register failed: %q", ack.Error)
	}
	slog.Info("connected to primary", "url", c.cfg.URL, "node_id", c.cfg.NodeID)

	// Enable WebSocket-level ping/pong for dead connection detection.
	// ReadDeadline resets on any pong response from the primary.
	const wsReadTimeout = 90 * time.Second
	conn.SetPongHandler(func(string) error {
		// SetReadDeadline error here means the conn was torn down between
		// the pong arrival and our refresh; surface it so the outer
		// ReadJSON loop exits via its error path instead of blocking.
		return conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	})
	if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
		return false, fmt.Errorf("set initial read deadline: %w", err)
	}

	return true, c.handleConn(ctx, conn)
}

// pingOnce runs a single WebSocket-level ping under writeMu and closes the
// conn on any failure. Returns true if the ping succeeded (caller keeps the
// ticker running), false if the conn was torn down (caller returns).
// Extracted from the ping-loop body so defer writeMu.Unlock() covers every
// exit path — the inline form had three separate manual Unlock sites that
// were easy to miss when adding a new failure branch.
func pingOnce(conn *websocket.Conn, writeMu *sync.Mutex) bool {
	writeMu.Lock()
	defer writeMu.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		_ = conn.Close()
		return false
	}
	if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
		_ = conn.Close()
		return false
	}
	return true
}

func (c *Connector) handleConn(ctx context.Context, conn *websocket.Conn) error {
	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		// If SetWriteDeadline fails (conn half-closed / already closed),
		// skip WriteJSON to avoid a deadline-less write that can block
		// until TCP keepalive expires. Return the underlying error so the
		// caller reconnects instead of silently hanging.
		if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
		return conn.WriteJSON(v)
	}

	// Limit concurrent request handling to avoid unbounded goroutine growth.
	reqSem := make(chan struct{}, 16)

	// connCtx is cancelled when this connection drops, ensuring stream
	// goroutines exit promptly without blocking reconnect.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	// activeSubs tracks local session subscriptions initiated by primary.
	// subExited receives keys when streamEvents goroutines exit (channel closed),
	// so the main loop can remove stale entries and allow re-subscription.
	// A generation counter prevents late subExited notifications from deleting
	// a freshly re-created subscription for the same key.
	type subExitNote struct {
		key string
		gen uint64
	}
	activeSubs := map[string]func(){} // key → cancel func
	subGen := map[string]uint64{}     // key → generation counter
	// Capacity 256: streamEvents goroutines drop their subExited note
	// non-blockingly, and the main loop drains between ReadJSON calls. A
	// 64-slot channel could overflow during hub-wide resets (e.g. Router
	// Cleanup sweeping >64 sessions at once while ReadJSON is blocked),
	// leaving stale activeSubs entries. 256 covers realistic burst sizes
	// for all deployments; the reqSem inflight cap is 16 so a 256-deep
	// backlog is a generous safety margin. R71-GO-M1.
	subExited := make(chan subExitNote, 256)

	var wg sync.WaitGroup
	// R51-REL-005: bound the shutdown-of-handleConn on a hard deadline so a
	// stuck worker goroutine (typically a send-RPC blocked on sess.Send that
	// can wait up to CLI watchdog timeout ≈ 5 min) cannot pin reconnect.
	// connCancel() above already fired by the time we reach this defer —
	// every wg participant either responds to connCtx or is inherently
	// short-running (ping ticker, response writer), so the grace timer
	// covers only the pathological case where a downstream Send refuses
	// to honour ctx. Exceeding the budget leaks the stuck goroutine to
	// process teardown, which is strictly better than blocking the whole
	// upstream reconnect loop.
	defer func() {
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(handleConnDrainBudget):
			slog.Warn("connector: handleConn drain exceeded budget, proceeding",
				"budget", handleConnDrainBudget)
		}
	}()

	// Periodically send WebSocket-level pings so pongHandler resets the read deadline.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Hold writeMu across the Close on failure so conn.Close does
				// not race with a concurrent writeJSON that has just entered
				// the critical section. gorilla/websocket requires at most
				// one writer at a time; closing under the lock serializes us
				// against WriteJSON. Any writeJSON that then acquires the
				// lock will see SetWriteDeadline fail (closed conn) and
				// return its error cleanly. Force-close is what breaks the
				// outer ReadJSON out of its 90s pong wait when the peer is
				// dead — we want that to happen even if no Write failed
				// yet, so emit the Close without unlocking first.
				//
				// pingOnce encapsulates the "lock → try write → close on
				// failure" triad in a single scope so a single `defer
				// writeMu.Unlock()` covers every exit. The boolean return
				// lets the outer loop exit without keeping the lock live
				// across the return.
				if !pingOnce(conn, &writeMu) {
					return
				}
			case <-connCtx.Done():
				return
			}
		}
	}()

	// Clean up all event log subscriptions when connection drops.
	defer func() {
		for key, cancel := range activeSubs {
			cancel()
			delete(activeSubs, key)
		}
	}()

	for {
		// Drain stale subscription entries from exited streamEvents goroutines
		// so re-subscribe messages for the same key are accepted.
	drainLoop:
		for {
			select {
			case note := <-subExited:
				if subGen[note.key] == note.gen {
					delete(activeSubs, note.key)
				}
			default:
				break drainLoop
			}
		}

		var msg node.ReverseMsg
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}

		switch msg.Type {
		case "request":
			req := msg
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						slog.Error("connector request panic", "req_id", req.ReqID, "method", req.Method, "panic", r, "stack", string(debug.Stack()))
					}
				}()
				select {
				case reqSem <- struct{}{}:
					defer func() { <-reqSem }()
				case <-ctx.Done():
					return
				}
				result, err := c.handleRequest(ctx, connCtx, req, &wg)
				resp := node.ReverseMsg{Type: "response", ReqID: req.ReqID}
				if err != nil {
					resp.Error = err.Error()
				} else {
					resp.Result = result
				}
				if wErr := writeJSON(resp); wErr != nil {
					slog.Debug("connector response write failed", "err", wErr)
				}
			}()

		case "subscribe":
			key := msg.Key
			// Cancel stale subscription if the previous streamEvents goroutine
			// exited (e.g. process died). This allows the hub to re-subscribe
			// after a remote send so events flow for the new process.
			if cancel, already := activeSubs[key]; already {
				cancel()
				delete(activeSubs, key)
			}
			sess := c.router.GetSession(key)
			if sess == nil {
				if err := writeJSON(node.ReverseMsg{Type: "subscribe_error", Key: key, Error: "session not found"}); err != nil {
					slog.Debug("connector write subscribe_error", "key", key, "err", err)
				}
				break
			}
			notify, cancel := sess.SubscribeEvents()
			activeSubs[key] = cancel
			subGen[key]++
			myGen := subGen[key]
			if err := writeJSON(node.ReverseMsg{Type: "subscribed", Key: key}); err != nil {
				slog.Debug("connector write subscribed", "key", key, "err", err)
			}
			wg.Add(1)
			go func(k string, n <-chan struct{}, g uint64) {
				defer wg.Done()
				c.streamEvents(connCtx, writeJSON, k, n)
				// Signal that this subscription exited (session replaced/reset).
				// A dropped notification leaves activeSubs[k] populated until
				// the next explicit subscribe/unsubscribe for the same key
				// clears it — not a correctness bug (cancel is idempotent),
				// but observability for capacity tuning. R71-GO-M1.
				select {
				case subExited <- subExitNote{k, g}:
				default:
					slog.Warn("connector: subExited channel full, activeSubs cleanup delayed", "key", k)
				}
			}(key, notify, myGen)

		case "unsubscribe":
			key := msg.Key
			if cancel, ok := activeSubs[key]; ok {
				cancel()
				delete(activeSubs, key)
			}
			if err := writeJSON(node.ReverseMsg{Type: "unsubscribed", Key: key}); err != nil {
				slog.Debug("connector write unsubscribed", "key", key, "err", err)
			}

		case "ping":
			if err := writeJSON(node.ReverseMsg{Type: "pong"}); err != nil {
				slog.Debug("connector write pong", "err", err)
			}
		}
	}
}

func (c *Connector) handleRequest(appCtx, connCtx context.Context, req node.ReverseMsg, wg *sync.WaitGroup) (json.RawMessage, error) {
	switch req.Method {
	case "fetch_sessions":
		return marshalResult(c.router.ListSessions())

	case "fetch_projects":
		if c.projMgr == nil {
			return marshalResult([]any{})
		}
		return marshalResult(c.projMgr.All())

	case "fetch_discovered":
		if c.discoverFunc != nil {
			return c.discoverFunc()
		}
		return marshalResult([]any{})

	case "fetch_discovered_preview":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("fetch_discovered_preview params: %w", err)
		}
		// Defense-in-depth: the HTTP dashboard path validates on the
		// control-node side and `discovery.LoadHistoryChainTailCtx` also
		// validates internally, but validating here at the RPC boundary
		// mirrors the `takeover` / `close_discovered` handlers and prevents
		// a future refactor from removing the internal check and exposing
		// `{".."}` / path-traversal inputs from a compromised primary.
		// R65-SEC-M-1.
		if p.SessionID != "" && !discovery.IsValidSessionID(p.SessionID) {
			return nil, fmt.Errorf("invalid session_id format")
		}
		if c.previewFunc != nil {
			return c.previewFunc(p.SessionID)
		}
		return marshalResult([]any{})

	case "fetch_events":
		var p struct {
			Key   string `json:"key"`
			After int64  `json:"after"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("fetch_events params: %w", err)
		}
		if err := session.ValidateSessionKey(p.Key); err != nil {
			return nil, fmt.Errorf("fetch_events key: %w", err)
		}
		sess := c.router.GetSession(p.Key)
		if sess == nil {
			return nil, fmt.Errorf("session not found: %s", p.Key)
		}
		return marshalResult(sess.EventEntriesSince(p.After))

	case "send":
		var p struct {
			Key       string `json:"key"`
			Text      string `json:"text"`
			Workspace string `json:"workspace"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("send params: %w", err)
		}
		if err := session.ValidateSessionKey(p.Key); err != nil {
			return nil, fmt.Errorf("send key: %w", err)
		}
		// Reject oversized text at the reverse-RPC trust boundary before it
		// reaches sess.Send → CoalesceMessages. Without this a compromised or
		// misconfigured primary could push up to ~16 MB (the WS read cap)
		// straight into CLI stdin, relying only on the shim's 12 MB line
		// ceiling to reject it. Matches the primary-side dashboard cap
		// chain (maxWSSendTextBytes=1 MB → coalesce soft cap 4 MB → shim
		// 12 MB). R68-SEC-H1.
		if n := len(p.Text); n > dispatch.MaxCoalescedTextBytes() {
			return nil, fmt.Errorf("send text too long: %d bytes", n)
		}
		opts := session.AgentOpts{}
		if p.Workspace != "" {
			// Syntactic pre-check before filepath.Clean/EvalSymlinks. Clean
			// silently folds `/home/../etc` into `/etc`, so a post-Clean
			// prefix check under an empty defaultWorkspace would let any
			// absolute path through on single-user deployments. R68-SEC-M2.
			if err := session.ValidateRemoteWorkspacePath(p.Workspace); err != nil {
				return nil, fmt.Errorf("workspace path invalid: %w", err)
			}
			// When no allowed root is configured (defaultWorkspace=="") on this
			// reverse node, we cannot bound the workspace to any prefix. A
			// compromised/misconfigured primary could otherwise push any
			// absolute path (e.g. `/etc`) and spawn a CLI session rooted
			// there. Refuse rather than trust. R68-SEC-M2.
			if c.defaultWorkspace == "" {
				return nil, fmt.Errorf("workspace overrides disabled: no allowed root configured on this node")
			}
			// Sanitize workspace path to prevent directory traversal via symlinks.
			ws, err := filepath.EvalSymlinks(filepath.Clean(p.Workspace))
			if err != nil {
				return nil, fmt.Errorf("workspace path invalid: %w", err)
			}
			if !filepath.IsAbs(ws) {
				return nil, fmt.Errorf("workspace must be absolute path")
			}
			if ws != c.defaultWorkspace &&
				!strings.HasPrefix(ws, c.defaultWorkspace+string(filepath.Separator)) {
				return nil, fmt.Errorf("workspace %q outside allowed root %q", ws, c.defaultWorkspace)
			}
			opts.Workspace = ws
		}
		sess, _, err := c.router.GetOrCreate(connCtx, p.Key, opts)
		if err != nil {
			return nil, fmt.Errorf("get session: %w", err)
		}
		// Send is async: primary subscribed before sending, events arrive via streamEvents.
		// Use connCtx so a relay disconnect cancels in-flight sends, preventing
		// goroutine accumulation across reconnect cycles. Register with the
		// handleConn waitgroup so a dropped connection waits for in-flight
		// sends to return before tearing down subscriptions.
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("connector send panic", "key", p.Key, "panic", r, "stack", string(debug.Stack()))
				}
			}()
			if _, err := sess.Send(connCtx, p.Text, nil, nil); err != nil {
				if connCtx.Err() == nil {
					slog.Warn("connector send failed", "key", p.Key, "err", err)
					// R49-REL-CONNECTOR-SEND-RESULT-LOSS: the RPC already
					// returned {"status":"accepted"} to primary, so a plain
					// log.Warn leaves the UI showing "sent" while the message
					// actually failed. Inject a system event into this
					// session's EventLog so subscribed dashboards surface
					// the failure on the next event push. Keep the message
					// compact and classifier-friendly; full detail stays
					// in the slog line above.
					//
					// R172-SEC-M4: err.Error() originates from a remote /
					// transport stack — it may contain C1 controls, bidi
					// overrides, or LS/PS characters that byte-level
					// `< 0x20` gates miss. The summary is broadcast to every
					// dashboard WS client subscribed to this session AND
					// appended to persistedHistory (journalctl / sessions
					// persistence), so an unsanitized error string here is a
					// log-injection primitive that can flip terminal output
					// under `tail -f` for operators and pollute dashboard
					// rendering. Sanitize through osutil.SanitizeForLog and
					// cap at 512 bytes — long remote stack traces beyond
					// that point add noise without diagnostic value (full
					// detail is already in the slog.Warn above).
					sess.LogSystemEvent("发送失败：" + osutil.SanitizeForLog(err.Error(), 512))
				}
			}
		}()
		return marshalResult(map[string]string{"status": "accepted"})

	case "takeover":
		var p struct {
			PID           int    `json:"pid"`
			SessionID     string `json:"session_id"`
			CWD           string `json:"cwd"`
			ProcStartTime uint64 `json:"proc_start_time"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("takeover params: %w", err)
		}
		if p.PID <= 0 || p.SessionID == "" {
			return nil, fmt.Errorf("pid and session_id are required")
		}
		if !discovery.IsValidSessionID(p.SessionID) {
			return nil, fmt.Errorf("invalid session_id format")
		}
		if p.ProcStartTime == 0 {
			return nil, fmt.Errorf("proc_start_time is required")
		}
		actual, err := discovery.ProcStartTime(p.PID)
		if err != nil {
			return nil, fmt.Errorf("cannot verify process identity for pid %d: %w", p.PID, err)
		}
		if actual != p.ProcStartTime {
			return nil, fmt.Errorf("process identity mismatch (pid %d may have been reused)", p.PID)
		}
		if err := syscall.Kill(p.PID, syscall.SIGTERM); err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				return nil, fmt.Errorf("kill process %d: %w", p.PID, err)
			}
		}
		cwd := p.CWD
		if cwd == "" {
			cwd = "unknown"
		}
		// Validate CWD against workspace root (same check as "send" RPC).
		if cwd != "unknown" {
			// Syntactic pre-check always — even with empty defaultWorkspace,
			// `..` traversal / control bytes / non-absolute paths have no
			// business reaching filepath.Clean. R68-SEC-M2.
			if err := session.ValidateRemoteWorkspacePath(cwd); err != nil {
				return nil, fmt.Errorf("takeover cwd invalid: %w", err)
			}
			// When no allowed root is configured on this reverse node, refuse
			// the cwd override — the takeover would otherwise spawn a CLI
			// session rooted wherever the primary pointed. Aligns with the
			// "send" RPC above so both call sites have the same policy.
			// R68-SEC-M2.
			if c.defaultWorkspace == "" {
				return nil, fmt.Errorf("takeover cwd overrides disabled: no allowed root configured on this node")
			}
			cleanCWD, err := filepath.EvalSymlinks(filepath.Clean(cwd))
			if err != nil {
				return nil, fmt.Errorf("takeover cwd path invalid: %w", err)
			}
			if !filepath.IsAbs(cleanCWD) {
				return nil, fmt.Errorf("takeover cwd must be absolute path")
			}
			if cleanCWD != c.defaultWorkspace &&
				!strings.HasPrefix(cleanCWD, c.defaultWorkspace+string(filepath.Separator)) {
				return nil, fmt.Errorf("takeover cwd %q outside allowed root %q", cleanCWD, c.defaultWorkspace)
			}
			cwd = cleanCWD
		}
		cwdKey := session.SanitizeCWDKey(cwd)
		key := session.TakeoverKey(cwdKey)
		pid, sessionID, procStartTime, reqCWD, claudeDir := p.PID, p.SessionID, p.ProcStartTime, p.CWD, c.claudeDir
		// Track with connection wg so reconnect waits for in-flight cleanup rather
		// than letting goroutines pile up across reconnect cycles. Use appCtx so a
		// transient connection drop does not abort cleanup already in progress;
		// appCtx outlives connCtx, but wg keeps accounting honest.
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("connector takeover panic", "key", key, "panic", r, "stack", string(debug.Stack()))
				}
			}()
			discovery.WaitAndCleanup(appCtx, pid, procStartTime, claudeDir, reqCWD, sessionID)
			if appCtx.Err() != nil {
				return // connector shutting down
			}
			if _, err := c.router.Takeover(appCtx, key, sessionID, cwd, session.AgentOpts{}); err != nil {
				slog.Debug("connector takeover failed", "key", key, "err", err)
			}
		}()
		return marshalResult(map[string]string{"status": "accepted", "key": key})

	case "close_discovered":
		// Proxied from primary's handleClose — no discovered-cache check here:
		// the primary already verified PID ∈ discovered before forwarding, and
		// the RPC caller is an authenticated node. ProcStartTime still guards
		// against PID reuse between primary's check and this kill.
		var p struct {
			PID           int    `json:"pid"`
			SessionID     string `json:"session_id"`
			CWD           string `json:"cwd"`
			ProcStartTime uint64 `json:"proc_start_time"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("close_discovered params: %w", err)
		}
		if p.PID <= 0 {
			return nil, fmt.Errorf("pid is required")
		}
		if p.ProcStartTime == 0 {
			return nil, fmt.Errorf("proc_start_time is required")
		}
		if p.SessionID != "" && !discovery.IsValidSessionID(p.SessionID) {
			return nil, fmt.Errorf("invalid session_id format")
		}
		// CWD flows into discovery.WaitAndCleanup which builds a lockDir path
		// and os.RemoveAll it (protected by filepath.Rel sandbox, but we still
		// reject syntactic `..` traversal / control bytes / non-absolute paths
		// up front to avoid depending on a single defense layer). Parallels the
		// takeover-side check at line 520. R-close-discovered-cwd-validate.
		if p.CWD != "" {
			if err := session.ValidateRemoteWorkspacePath(p.CWD); err != nil {
				return nil, fmt.Errorf("close_discovered cwd invalid: %w", err)
			}
		}
		actual, err := discovery.ProcStartTime(p.PID)
		if err != nil {
			return nil, fmt.Errorf("cannot verify process identity for pid %d: %w", p.PID, err)
		}
		if actual != p.ProcStartTime {
			return nil, fmt.Errorf("process identity mismatch (pid %d may have been reused)", p.PID)
		}
		if err := syscall.Kill(p.PID, syscall.SIGTERM); err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				return nil, fmt.Errorf("kill process %d: %w", p.PID, err)
			}
		}
		pid, sessionID, procStartTime, cwd, claudeDir := p.PID, p.SessionID, p.ProcStartTime, p.CWD, c.claudeDir
		// Track with connection wg so reconnect waits for this cleanup to finish.
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("connector close_discovered panic", "pid", pid, "panic", r, "stack", string(debug.Stack()))
				}
			}()
			if appCtx.Err() != nil {
				return
			}
			discovery.WaitAndCleanup(appCtx, pid, procStartTime, claudeDir, cwd, sessionID)
		}()
		return marshalResult(map[string]string{"status": "ok"})

	case "restart_planner":
		var p struct {
			ProjectName string `json:"project_name"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("restart_planner params: %w", err)
		}
		if c.projMgr == nil {
			return nil, fmt.Errorf("projects not configured")
		}
		proj := c.projMgr.Get(p.ProjectName)
		if proj == nil {
			return nil, fmt.Errorf("project not found: %s", p.ProjectName)
		}
		plannerKey := proj.PlannerSessionKey()
		opts := session.AgentOpts{
			Model:     c.projMgr.EffectivePlannerModel(proj),
			Workspace: proj.Path,
			Exempt:    true,
		}
		if prompt := c.projMgr.EffectivePlannerPrompt(proj); prompt != "" {
			opts.ExtraArgs = []string{"--append-system-prompt", prompt}
		}
		if _, err := c.router.ResetAndRecreate(connCtx, plannerKey, opts); err != nil {
			return nil, fmt.Errorf("restart planner: %w", err)
		}
		return marshalResult(map[string]string{"status": "restarted"})

	case "update_config":
		var p struct {
			ProjectName string          `json:"project_name"`
			Config      json.RawMessage `json:"config"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("update_config params: %w", err)
		}
		if c.projMgr == nil {
			return nil, fmt.Errorf("projects not configured")
		}
		var cfg project.ProjectConfig
		if err := json.Unmarshal(p.Config, &cfg); err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
		// Same validation the dashboard HTTP handler enforces: a compromised
		// or misconfigured primary must not be able to push unbounded prompts,
		// NUL-truncated argv, or flag-injected model names through the
		// reverse-RPC trust boundary. R68-SEC-H2.
		if err := project.ValidateConfig(cfg); err != nil {
			return nil, err
		}
		if err := c.projMgr.UpdateConfig(p.ProjectName, cfg); err != nil {
			return nil, fmt.Errorf("update config: %w", err)
		}
		return marshalResult(map[string]string{"status": "ok"})

	case "remove_session":
		var p struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("remove_session params: %w", err)
		}
		if err := session.ValidateSessionKey(p.Key); err != nil {
			return nil, fmt.Errorf("remove_session key: %w", err)
		}
		removed := c.router.Remove(p.Key)
		return marshalResult(map[string]bool{"removed": removed})

	case "interrupt_session":
		var p struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("interrupt_session params: %w", err)
		}
		if err := session.ValidateSessionKey(p.Key); err != nil {
			return nil, fmt.Errorf("interrupt_session key: %w", err)
		}
		// Prefer the non-destructive control_request path so the CLI
		// subprocess survives. Raw SIGINT via InterruptSession kills Claude
		// `-p` outright, which tears down the shim and forces a brand-new
		// spawn on the next message (losing resume context and leaking
		// socket files). Matches the dashboard HTTP / WS handlers. R67-GO-2.
		outcome := c.router.InterruptSessionSafe(p.Key)
		interrupted := outcome == session.InterruptSent
		return marshalResult(map[string]bool{"interrupted": interrupted})

	case "set_session_label":
		var p struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("set_session_label params: %w", err)
		}
		if err := session.ValidateSessionKey(p.Key); err != nil {
			return nil, fmt.Errorf("set_session_label key: %w", err)
		}
		// Full validation (length + UTF-8 + C0/C1 control gate) via the
		// shared validator. The dashboard-facing HTTP path already validates
		// on the control-node side; this second check defends the
		// server-role node against a compromised control-node shipping
		// labels with log-injection or terminal-corruption bytes directly
		// to the reverse-RPC worker. R64-GO-H3 / L1.
		label, err := session.ValidateUserLabel(p.Label)
		if err != nil {
			return nil, fmt.Errorf("set_session_label label: %w", err)
		}
		updated := c.router.SetUserLabel(p.Key, label)
		return marshalResult(map[string]bool{"updated": updated})

	case "set_favorite":
		var p struct {
			ProjectName string `json:"project_name"`
			Favorite    bool   `json:"favorite"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("set_favorite params: %w", err)
		}
		if c.projMgr == nil {
			return nil, fmt.Errorf("projects not configured")
		}
		if err := c.projMgr.SetFavorite(p.ProjectName, p.Favorite); err != nil {
			return nil, fmt.Errorf("set favorite: %w", err)
		}
		return marshalResult(map[string]any{"status": "ok", "favorite": p.Favorite})

	default:
		return nil, fmt.Errorf("unknown method: %s", req.Method)
	}
}

func (c *Connector) streamEvents(ctx context.Context, writeJSON func(any) error, key string, notify <-chan struct{}) {
	sess := c.router.GetSession(key)
	if sess == nil {
		return
	}
	var lastTime int64
	var lastState string
	for {
		select {
		case _, ok := <-notify:
			if !ok {
				// Session was reset/replaced; the notify channel is closed.
				// Send final state so the hub knows the process died and can
				// trigger a re-subscribe when the next send arrives.
				if s := c.router.GetSession(key); s != nil {
					snap := s.Snapshot()
					if err := writeJSON(node.ReverseMsg{Type: "session_state", Key: key, State: snap.State, Reason: snap.DeathReason}); err != nil {
						slog.Debug("connector write final session_state", "key", key, "err", err)
					}
				}
				return
			}
			// Re-fetch session in case it was replaced (e.g. via /new). A
			// replaced session has a fresh event log whose wall-clock
			// timestamps can be earlier than the old lastTime (NTP jumps or
			// fast /new), causing EntriesSince to drop the new session's
			// first events. Reset lastTime on pointer change so the first
			// notify after a swap delivers the full new history.
			if cur := c.router.GetSession(key); cur != nil && cur != sess {
				sess = cur
				lastTime = 0
				lastState = ""
			}
			entries := sess.EventEntriesSince(lastTime)
			if len(entries) > 0 {
				if err := writeJSON(node.ReverseMsg{Type: "events", Key: key, Events: entries}); err != nil {
					return
				}
				// entries are chronological; last entry has the highest timestamp
				lastTime = entries[len(entries)-1].Time
			}
			// Only push session_state when it actually changes
			snap := sess.Snapshot()
			if snap.State != lastState {
				lastState = snap.State
				if err := writeJSON(node.ReverseMsg{Type: "session_state", Key: key, State: snap.State, Reason: snap.DeathReason}); err != nil {
					slog.Debug("connector write session_state", "key", key, "err", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func marshalResult(v any) (json.RawMessage, error) {
	return json.Marshal(v)
}
