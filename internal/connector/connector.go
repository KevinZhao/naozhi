package connector

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
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/reverse"
	"github.com/naozhi/naozhi/internal/session"
)

// Connector dials a primary naozhi and serves it as a reverse-connected node.
// Run on machines behind NAT that cannot be reached by the primary directly.
type Connector struct {
	cfg          *config.UpstreamConfig
	router       *session.Router
	projMgr      *project.Manager // may be nil
	claudeDir    string
	hostname     string
	discoverFunc func() (json.RawMessage, error)
	previewFunc  func(sessionID string) (json.RawMessage, error)
}

// New creates a Connector. projMgr may be nil if projects are not configured.
func New(cfg *config.UpstreamConfig, router *session.Router, projMgr *project.Manager) *Connector {
	claudeDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		claudeDir = filepath.Join(home, ".claude")
	}
	hostname, _ := os.Hostname()
	return &Connector{cfg: cfg, router: router, projMgr: projMgr, claudeDir: claudeDir, hostname: hostname}
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
	reg := reverse.ReverseMsg{
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
	var ack reverse.ReverseMsg
	if err := conn.ReadJSON(&ack); err != nil {
		return false, fmt.Errorf("register ack read: %w", err)
	}
	conn.SetReadDeadline(time.Time{})

	if ack.Type != "registered" {
		return false, fmt.Errorf("register failed: %s", ack.Error)
	}
	slog.Info("connected to primary", "url", c.cfg.URL, "node_id", c.cfg.NodeID)

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

	// activeSubs tracks local session subscriptions initiated by primary
	activeSubs := map[string]func(){} // key → cancel func

	var wg sync.WaitGroup
	defer wg.Wait()

	// Clean up all event log subscriptions when connection drops.
	defer func() {
		for key, cancel := range activeSubs {
			cancel()
			delete(activeSubs, key)
		}
	}()

	for {
		var msg reverse.ReverseMsg
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
				result, err := c.handleRequest(connCtx, req)
				resp := reverse.ReverseMsg{Type: "response", ReqID: req.ReqID}
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
			if _, already := activeSubs[key]; already {
				break
			}
			sess := c.router.GetSession(key)
			if sess == nil {
				writeJSON(reverse.ReverseMsg{Type: "subscribe_error", Key: key, Error: "session not found"}) //nolint
				break
			}
			notify, cancel := sess.SubscribeEvents()
			activeSubs[key] = cancel
			writeJSON(reverse.ReverseMsg{Type: "subscribed", Key: key}) //nolint
			wg.Add(1)
			go func(k string, n <-chan struct{}) {
				defer wg.Done()
				c.streamEvents(connCtx, writeJSON, k, n)
			}(key, notify)

		case "unsubscribe":
			key := msg.Key
			if cancel, ok := activeSubs[key]; ok {
				cancel()
				delete(activeSubs, key)
			}
			writeJSON(reverse.ReverseMsg{Type: "unsubscribed", Key: key}) //nolint

		case "ping":
			writeJSON(reverse.ReverseMsg{Type: "pong"}) //nolint
		}
	}
}

func (c *Connector) handleRequest(ctx context.Context, req reverse.ReverseMsg) (json.RawMessage, error) {
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
			// Sanitize workspace path to prevent directory traversal.
			// Primary has already validated against allowedRoot, but we
			// still clean the path to avoid ".." injection.
			ws := filepath.Clean(p.Workspace)
			if !filepath.IsAbs(ws) {
				return nil, fmt.Errorf("workspace must be absolute path")
			}
			opts.Workspace = ws
		}
		sess, _, err := c.router.GetOrCreate(ctx, p.Key, opts)
		if err != nil {
			return nil, fmt.Errorf("get session: %w", err)
		}
		// Send is async: primary subscribed before sending, events arrive via streamEvents
		go func() {
			if _, err := sess.Send(ctx, p.Text, nil, nil); err != nil {
				slog.Debug("connector send failed", "key", p.Key, "err", err)
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
		if actual, err := discovery.ProcStartTime(p.PID); err != nil || actual != p.ProcStartTime {
			return nil, fmt.Errorf("process identity mismatch")
		}
		if err := syscall.Kill(p.PID, syscall.SIGTERM); err != nil {
			return nil, fmt.Errorf("kill process %d: %w", p.PID, err)
		}
		cwd := p.CWD
		if cwd == "" {
			cwd = "unknown"
		}
		cwdKey := strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
		key := "local:takeover:" + cwdKey + ":general"
		pid, sessionID, procStartTime, reqCWD, claudeDir := p.PID, p.SessionID, p.ProcStartTime, p.CWD, c.claudeDir
		go func() {
			discovery.WaitAndCleanup(pid, procStartTime, claudeDir, reqCWD, sessionID)
			if _, err := c.router.Takeover(context.Background(), key, sessionID, cwd, session.AgentOpts{}); err != nil {
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
		if _, err := c.router.ResetAndRecreate(ctx, plannerKey, opts); err != nil {
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
	for {
		select {
		case _, ok := <-notify:
			if !ok {
				return
			}
			entries := sess.EventEntriesSince(lastTime)
			for i := range entries {
				ev := entries[i]
				msg := reverse.ReverseMsg{Type: "event", Key: key, Event: &ev}
				if err := writeJSON(msg); err != nil {
					return
				}
				if ev.Time > lastTime {
					lastTime = ev.Time
				}
			}
			// Also push session_state when session state changes
			snap := sess.Snapshot()
			writeJSON(reverse.ReverseMsg{Type: "session_state", Key: key, State: snap.State}) //nolint
		case <-ctx.Done():
			return
		}
	}
}

func marshalResult(v any) (json.RawMessage, error) {
	return json.Marshal(v)
}
