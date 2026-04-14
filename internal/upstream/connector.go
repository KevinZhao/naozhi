package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

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
		timer := time.NewTimer(backoff)
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
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, dialErr := dialer.DialContext(ctx, c.cfg.URL, nil)
	if dialErr != nil {
		return false, fmt.Errorf("dial: %w", dialErr)
	}
	defer conn.Close()

	// Close the WebSocket when ctx is cancelled to unblock ReadJSON in handleConn.
	connDone := make(chan struct{})
	defer close(connDone)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
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

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var ack node.ReverseMsg
	if err := conn.ReadJSON(&ack); err != nil {
		return false, fmt.Errorf("register ack read: %w", err)
	}
	conn.SetReadDeadline(time.Time{})

	if ack.Type != "registered" {
		return false, fmt.Errorf("register failed: %s", ack.Error)
	}
	slog.Info("connected to primary", "url", c.cfg.URL, "node_id", c.cfg.NodeID)

	// Enable WebSocket-level ping/pong for dead connection detection.
	// ReadDeadline resets on any pong response from the primary.
	const wsReadTimeout = 90 * time.Second
	conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		return nil
	})

	return true, c.handleConn(ctx, conn)
}

func (c *Connector) handleConn(ctx context.Context, conn *websocket.Conn) error {
	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
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
	subExited := make(chan subExitNote, 16)

	var wg sync.WaitGroup
	defer wg.Wait()

	// Periodically send WebSocket-level pings so pongHandler resets the read deadline.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				writeMu.Lock()
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err := conn.WriteMessage(websocket.PingMessage, nil)
				writeMu.Unlock()
				if err != nil {
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
				select {
				case reqSem <- struct{}{}:
					defer func() { <-reqSem }()
				case <-ctx.Done():
					return
				}
				result, err := c.handleRequest(ctx, connCtx, req)
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
				select {
				case subExited <- subExitNote{k, g}:
				default:
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

func (c *Connector) handleRequest(appCtx, connCtx context.Context, req node.ReverseMsg) (json.RawMessage, error) {
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
		opts := session.AgentOpts{}
		if p.Workspace != "" {
			// Sanitize workspace path to prevent directory traversal via symlinks.
			ws, err := filepath.EvalSymlinks(filepath.Clean(p.Workspace))
			if err != nil {
				return nil, fmt.Errorf("workspace path invalid: %w", err)
			}
			if !filepath.IsAbs(ws) {
				return nil, fmt.Errorf("workspace must be absolute path")
			}
			if c.defaultWorkspace != "" && ws != c.defaultWorkspace &&
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
		// Use the application-level appCtx (not connCtx) so a relay connection drop
		// does not kill the in-progress Claude process via context cancellation.
		go func() {
			if _, err := sess.Send(appCtx, p.Text, nil, nil); err != nil {
				slog.Warn("connector send failed", "key", p.Key, "err", err)
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
			return nil, fmt.Errorf("kill process %d: %w", p.PID, err)
		}
		cwd := p.CWD
		if cwd == "" {
			cwd = "unknown"
		}
		cwdKey := strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
		cwdKey = strings.ReplaceAll(cwdKey, ":", "_")
		if len(cwdKey) > 128 {
			cwdKey = cwdKey[:128]
		}
		key := session.TakeoverKey(cwdKey)
		pid, sessionID, procStartTime, reqCWD, claudeDir := p.PID, p.SessionID, p.ProcStartTime, p.CWD, c.claudeDir
		go func() {
			discovery.WaitAndCleanup(pid, procStartTime, claudeDir, reqCWD, sessionID)
			if appCtx.Err() != nil {
				return // connector shutting down
			}
			if _, err := c.router.Takeover(appCtx, key, sessionID, cwd, session.AgentOpts{}); err != nil {
				slog.Debug("connector takeover failed", "key", key, "err", err)
			}
		}()
		return marshalResult(map[string]string{"status": "accepted", "key": key})

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
		if err := c.projMgr.UpdateConfig(p.ProjectName, cfg); err != nil {
			return nil, fmt.Errorf("update config: %w", err)
		}
		return marshalResult(map[string]string{"status": "ok"})

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
			// Re-fetch session in case it was replaced (e.g. via /new).
			if cur := c.router.GetSession(key); cur != nil {
				sess = cur
			}
			entries := sess.EventEntriesSince(lastTime)
			if len(entries) > 0 {
				if err := writeJSON(node.ReverseMsg{Type: "events", Key: key, Events: entries}); err != nil {
					return
				}
				for i := range entries {
					if entries[i].Time > lastTime {
						lastTime = entries[i].Time
					}
				}
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
