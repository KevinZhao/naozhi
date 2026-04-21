package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	relayReadTimeout  = 90 * time.Second
	relayPingInterval = 30 * time.Second
)

// wsRelay maintains a persistent WS connection to a remote node
// and forwards events to local browser clients.
type wsRelay struct {
	node      *HTTPClient
	nodeField []byte // pre-computed `"node":"<id>",` bytes for raw injection
	mu        sync.Mutex
	writeMu   sync.Mutex // serializes writes to the WS connection
	conn      *websocket.Conn
	connReady chan struct{}          // non-nil while a dial is in progress; closed when done
	subs      map[string][]EventSink // remote session key -> local clients
	lastEvent map[string]int64       // key -> last event unix ms (for reconnect)
	done      chan struct{}
	closed    bool
}

func newWSRelay(node *HTTPClient) *wsRelay {
	// Pre-compute the JSON field injection bytes once per relay.
	nodeJSON, _ := json.Marshal(node.ID)
	nodeField := []byte(`"node":` + string(nodeJSON) + `,`)
	return &wsRelay{
		node:      node,
		nodeField: nodeField,
		subs:      make(map[string][]EventSink),
		lastEvent: make(map[string]int64),
		done:      make(chan struct{}),
	}
}

// Subscribe subscribes a local client to a remote session key.
// Connects to the remote node on first call.
func (r *wsRelay) Subscribe(c EventSink, key string, after int64) {
	if err := r.ensureConnected(); err != nil {
		c.SendJSON(ServerMsg{Type: "error", Key: key, Node: r.node.ID, Error: "relay connect: " + err.Error()})
		return
	}

	r.mu.Lock()
	alreadySubscribed := len(r.subs[key]) > 0
	r.subs[key] = append(r.subs[key], c)
	r.mu.Unlock()

	if alreadySubscribed {
		// Key already subscribed on remote; send history via HTTP to just this client
		go r.sendHistoryToClient(c, key, after)
		return
	}

	// First subscriber for this key: subscribe on the remote WS
	r.writeJSON(ClientMsg{Type: "subscribe", Key: key, After: after})
}

// Unsubscribe removes a local client from a remote session key.
func (r *wsRelay) Unsubscribe(c EventSink, key string) {
	r.mu.Lock()
	empty := removeSub(r.subs, key, c)
	if empty {
		delete(r.lastEvent, key)
	}
	r.mu.Unlock()

	if empty {
		r.writeJSON(ClientMsg{Type: "unsubscribe", Key: key})
	}
	c.SendJSON(ServerMsg{Type: "unsubscribed", Key: key, Node: r.node.ID})
}

// RemoveClient removes a client from all subscriptions (called on disconnect).
func (r *wsRelay) RemoveClient(c EventSink) {
	r.mu.Lock()
	emptyKeys := removeSubAll(r.subs, c)
	for _, key := range emptyKeys {
		delete(r.lastEvent, key)
	}
	r.mu.Unlock()

	for _, key := range emptyKeys {
		r.writeJSON(ClientMsg{Type: "unsubscribe", Key: key})
	}
}

// Close closes the WS connection and cleans up.
func (r *wsRelay) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.done)
	conn := r.conn
	r.conn = nil
	r.subs = make(map[string][]EventSink)
	r.lastEvent = make(map[string]int64)
	r.mu.Unlock()

	if conn != nil {
		conn.Close()
	}
}

func (r *wsRelay) ensureConnected() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("relay closed")
	}
	if r.conn != nil {
		r.mu.Unlock()
		return nil
	}
	if r.connReady != nil {
		// Another goroutine is connecting; wait for it to finish.
		ch := r.connReady
		r.mu.Unlock()
		<-ch
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.conn != nil {
			return nil
		}
		return fmt.Errorf("connection attempt failed")
	}
	// We are the dialer.
	r.connReady = make(chan struct{})
	r.mu.Unlock()

	err := r.connect()

	r.mu.Lock()
	close(r.connReady)
	r.connReady = nil
	r.mu.Unlock()

	return err
}

func (r *wsRelay) connect() error {
	// Use CutPrefix to avoid mid-URL mismatches: a host like
	// "example.com/path-with-http://" would otherwise be mangled. Order
	// matters — CutPrefix("https://") must come first since "http://" is
	// also a prefix of "https://".
	var wsURL string
	switch {
	case strings.HasPrefix(r.node.URL, "https://"):
		wsURL = "wss://" + strings.TrimPrefix(r.node.URL, "https://")
	case strings.HasPrefix(r.node.URL, "http://"):
		wsURL = "ws://" + strings.TrimPrefix(r.node.URL, "http://")
	default:
		return fmt.Errorf("relay %s: unsupported URL scheme: %s", r.node.ID, r.node.URL)
	}
	wsURL += "/ws"

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		// Pin TLS floor to 1.2; node-to-node traffic over wss:// must never
		// accept legacy protocol versions regardless of Go default drift.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", r.node.ID, err)
	}

	// Authenticate
	if err := conn.WriteJSON(ClientMsg{Type: "auth", Token: r.node.Token}); err != nil {
		conn.Close()
		return fmt.Errorf("auth write %s: %w", r.node.ID, err)
	}
	var resp ServerMsg
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&resp); err != nil {
		conn.Close()
		return fmt.Errorf("auth read %s: %w", r.node.ID, err)
	}
	if resp.Type != "auth_ok" {
		conn.Close()
		return fmt.Errorf("auth failed %s: %s", r.node.ID, resp.Error)
	}
	conn.SetReadDeadline(time.Time{})

	// Detect silent disconnections (NAT timeout, crash without FIN/RST)
	// via read deadline + pong handler, matching reverseconn.go pattern.
	conn.SetReadDeadline(time.Now().Add(relayReadTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(relayReadTimeout))
		return nil
	})

	r.mu.Lock()
	if r.conn != nil || r.closed {
		// Another goroutine already connected, or Close() ran during our dial.
		// Either way, drop this conn rather than store it and spawn goroutines.
		r.mu.Unlock()
		conn.Close()
		return nil
	}
	r.conn = conn
	r.mu.Unlock()

	go r.pingLoop(conn)
	go r.readLoop(conn)
	return nil
}

func (r *wsRelay) readLoop(conn *websocket.Conn) {
	defer func() {
		r.mu.Lock()
		// Only nil out if this is still the active connection
		if r.conn == conn {
			r.conn = nil
		}
		shouldReconnect := r.conn == nil && !r.closed
		r.mu.Unlock()

		conn.Close()

		if !shouldReconnect {
			return
		}
		select {
		case <-r.done:
			return
		default:
		}
		go r.reconnect()
	}()

	for {
		select {
		case <-r.done:
			return
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(relayReadTimeout))

		// Parse only the key and type for routing + lastEvent tracking.
		// Avoid full unmarshal+remarshal by injecting the node field into raw bytes.
		var header struct {
			Type  string `json:"type"`
			Key   string `json:"key"`
			Event struct {
				Time int64 `json:"time"`
			} `json:"event"`
		}
		if json.Unmarshal(data, &header) != nil {
			continue
		}

		// Track last event time for reconnect resubscribe.
		// SendRaw is a non-blocking channel send; calling it under the lock is safe
		// and avoids a per-event snapshot slice allocation.
		tagged := injectNodeField(data, r.nodeField)
		r.mu.Lock()
		if header.Type == "event" && header.Event.Time > r.lastEvent[header.Key] {
			r.lastEvent[header.Key] = header.Event.Time
		}
		for _, c := range r.subs[header.Key] {
			c.SendRaw(tagged)
		}
		r.mu.Unlock()
	}
}

// injectNodeField inserts a pre-computed "node":"id", field into raw JSON bytes
// without full decode/encode. JSON objects always start with '{'.
// If the message already contains a "node" key, it is returned as-is to prevent
// duplicate-key ambiguity across JSON parsers.
func injectNodeField(data, nodeField []byte) []byte {
	if len(data) == 0 || data[0] != '{' {
		return data
	}
	// Skip injection if the remote message already has a "node" key.
	// Match `"node":` (with colon) to avoid false positives where "node" appears
	// only as a value inside another field.
	//
	// Window size: scans the whole payload. A short peek-window (previously
	// 256B) could miss the "node" key when it's encoded after a long session
	// key or large event body, causing double-injection → duplicate-key JSON
	// whose resolution is parser-defined (often last-wins, clobbering the
	// real node ID). A Contains scan on a ~KB to multi-KB slice is O(n) but
	// runs once per inbound message; the bytes package uses SIMD-friendly
	// search, so the cost is negligible versus the correctness gain.
	if bytes.Contains(data, []byte(`"node":`)) {
		return data
	}
	// Guard: empty object "{}" — nodeField ends with ',' which would produce
	// invalid JSON like {"node":"id",}. Strip the trailing comma instead.
	if len(data) == 2 {
		result := make([]byte, 0, 1+len(nodeField)-1+1)
		result = append(result, '{')
		result = append(result, nodeField[:len(nodeField)-1]...) // strip trailing ','
		result = append(result, '}')
		return result
	}
	result := make([]byte, 0, len(data)+len(nodeField))
	result = append(result, '{')
	result = append(result, nodeField...)
	result = append(result, data[1:]...)
	return result
}

// pingLoop sends periodic WebSocket pings to detect silent disconnections.
// WriteControl is safe to call concurrently with other write methods.
func (r *wsRelay) pingLoop(conn *websocket.Conn) {
	ticker := time.NewTicker(relayPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			r.mu.Lock()
			active := r.conn == conn
			r.mu.Unlock()
			if !active {
				return
			}
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
				return
			}
		}
	}
}

func (r *wsRelay) reconnect() {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		// Jitter the sleep so N relays reconnecting to the same primary after
		// a restart don't all hit the listener at identical offsets. backoff
		// itself keeps the doubling shape; jitter only scatters the wall-time
		// a single attempt fires at.
		t := time.NewTimer(jitterBackoff(backoff))
		select {
		case <-r.done:
			t.Stop()
			return
		case <-t.C:
		}

		if err := r.connect(); err != nil {
			slog.Warn("relay reconnect failed", "node", r.node.ID, "err", err)
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Resubscribe to all active keys with last-seen timestamps
		r.mu.Lock()
		type resub struct {
			key   string
			after int64
		}
		resubscribes := make([]resub, 0, len(r.subs))
		for key := range r.subs {
			if len(r.subs[key]) > 0 {
				resubscribes = append(resubscribes, resub{key, r.lastEvent[key]})
			}
		}
		r.mu.Unlock()

		for _, e := range resubscribes {
			r.writeJSON(ClientMsg{Type: "subscribe", Key: e.key, After: e.after})
		}
		slog.Info("relay reconnected", "node", r.node.ID, "keys", len(resubscribes))
		return
	}
}

func (r *wsRelay) sendHistoryToClient(c EventSink, key string, after int64) {
	c.SendJSON(ServerMsg{Type: "subscribed", Key: key, Node: r.node.ID})

	// Tie the 5s budget to the relay lifecycle so a Close() during fetch
	// aborts the HTTP call immediately instead of letting the goroutine
	// run for up to 5s and then send to a half-closed EventSink.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		select {
		case <-r.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	entries, err := r.node.FetchEvents(ctx, key, after)
	if err != nil {
		slog.Warn("relay fetch history", "node", r.node.ID, "key", key, "err", err)
		return
	}
	if len(entries) > 0 {
		c.SendJSON(ServerMsg{Type: "history", Key: key, Node: r.node.ID, Events: entries})
	}
}

// writeJSON sends a JSON message via the relay websocket.
// Lock ordering: writeMu → mu (never hold mu then acquire writeMu).
func (r *wsRelay) writeJSON(v any) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	conn := r.conn
	closed := r.closed
	r.mu.Unlock()
	if conn == nil || closed {
		return
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteJSON(v); err != nil {
		slog.Warn("relay write failed, closing connection for reconnect", "node", r.node.ID, "err", err)
		conn.Close() // triggers readLoop exit → automatic reconnect
	}
}
