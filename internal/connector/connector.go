package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/reverse"
	"github.com/naozhi/naozhi/internal/session"
)

// UpstreamConfig holds the connection details for dialling a primary naozhi instance.
// Mirror this into internal/config/config.go as config.UpstreamConfig when wiring up
// the full application.
type UpstreamConfig struct {
	URL         string `yaml:"url"`
	NodeID      string `yaml:"node_id"`
	Token       string `yaml:"token"`
	DisplayName string `yaml:"display_name"`
}

// Connector dials a primary naozhi and serves it as a reverse-connected node.
// Run on machines behind NAT that cannot be reached by the primary directly.
type Connector struct {
	cfg     *UpstreamConfig
	router  *session.Router
	projMgr *project.Manager // may be nil
}

// New creates a Connector. projMgr may be nil if projects are not configured.
func New(cfg *UpstreamConfig, router *session.Router, projMgr *project.Manager) *Connector {
	return &Connector{cfg: cfg, router: router, projMgr: projMgr}
}

// Run connects to the primary and serves requests. Reconnects on disconnect.
// Blocks until ctx is cancelled.
func (c *Connector) Run(ctx context.Context) {
	backoff := time.Second
	for {
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("connector disconnected", "url", c.cfg.URL, "err", err)
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

func (c *Connector) runOnce(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, c.cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
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
	}
	if err := conn.WriteJSON(reg); err != nil {
		return fmt.Errorf("register write: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var ack reverse.ReverseMsg
	if err := conn.ReadJSON(&ack); err != nil {
		return fmt.Errorf("register ack read: %w", err)
	}
	conn.SetReadDeadline(time.Time{})

	if ack.Type != "registered" {
		return fmt.Errorf("register failed: %s", ack.Error)
	}
	slog.Info("connected to primary", "url", c.cfg.URL, "node_id", c.cfg.NodeID)

	return c.handleConn(ctx, conn)
}

func (c *Connector) handleConn(ctx context.Context, conn *websocket.Conn) error {
	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(v)
	}

	// connCtx is cancelled when this connection drops, ensuring stream
	// goroutines exit promptly without blocking reconnect.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	// activeSubs tracks local session subscriptions initiated by primary
	activeSubs := map[string]func(){} // key → cancel func

	var wg sync.WaitGroup
	defer wg.Wait()

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
				result, err := c.handleRequest(ctx, req)
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
		// Discovered process scanning not available in connector context
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
			Key  string `json:"key"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, fmt.Errorf("send params: %w", err)
		}
		sess, _, err := c.router.GetOrCreate(ctx, p.Key, session.AgentOpts{})
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
