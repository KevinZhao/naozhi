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
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/osutil"
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
	// remoteDropped marks keys whose remote subscription the primary discarded
	// (session_state{reason:"subscription_timeout"}) while r.subs[key] is still
	// populated; without it Subscribe would take the alreadySubscribed branch
	// and never rebuild the remote subscription. Single-shot: the next
	// Subscribe re-sends `subscribe` and clears it (#2421).
	remoteDropped map[string]bool
	done          chan struct{}
	closed        bool
	// baseCtx unifies cancellation of in-flight sendHistoryToClient RPCs;
	// Close() fires baseCancel so FetchEvents unwinds without a watcher goroutine.
	baseCtx    context.Context
	baseCancel context.CancelFunc
	// wg tracks sendHistoryToClient goroutines so Close() never returns while
	// a history fetch could still SendJSON to a sink.
	wg sync.WaitGroup
	// reconnecting gates re-entrant reconnect loops: a writeJSON failure inside
	// a resubscribe closes the conn and readLoop's defer would otherwise spawn
	// a second concurrent reconnect (duplicate `subscribe` frames).
	reconnecting atomic.Bool
}

func newWSRelay(node *HTTPClient) *wsRelay {
	nodeJSON, _ := json.Marshal(node.ID)
	nodeField := []byte(`"node":` + string(nodeJSON) + `,`)
	baseCtx, baseCancel := context.WithCancel(context.Background())
	return &wsRelay{
		node:          node,
		nodeField:     nodeField,
		subs:          make(map[string][]EventSink),
		lastEvent:     make(map[string]int64),
		remoteDropped: make(map[string]bool),
		done:          make(chan struct{}),
		baseCtx:       baseCtx,
		baseCancel:    baseCancel,
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
	if r.closed {
		r.mu.Unlock()
		c.SendJSON(ServerMsg{Type: "error", Key: key, Node: r.node.ID, Error: "relay closed"})
		return
	}
	alreadySubscribed := len(r.subs[key]) > 0
	// Same client re-subscribing keeps exactly one entry.
	if !containsSink(r.subs[key], c) {
		r.subs[key] = append(r.subs[key], c)
	}
	// Whoever re-subscribes first after a remote drop rebuilds the WS
	// subscription; clear the marker so the rebuild happens once.
	rebuildRemote := alreadySubscribed && r.remoteDropped[key]
	delete(r.remoteDropped, key)
	// Seed lastEvent on first subscribe, or a reconnect() racing before the
	// first forwarded event would resend after=0 and replay full history.
	// Later subscribers must not regress the seed.
	if !alreadySubscribed {
		r.lastEvent[key] = after
	}
	// wg.Add under r.mu so a Close() that sees r.closed also sees the Add.
	historyOnly := alreadySubscribed && !rebuildRemote
	if historyOnly {
		r.wg.Add(1)
	}
	r.mu.Unlock()

	if historyOnly {
		go r.sendHistoryToClient(c, key, after)
		return
	}

	// First subscriber or remote rebuild: the remote answers with `subscribed`
	// + an Initial history frame that readLoop fans out to every local subscriber.
	r.writeJSON(ClientMsg{Type: "subscribe", Key: key, After: after})
}

// Unsubscribe removes a local client from a remote session key.
func (r *wsRelay) Unsubscribe(c EventSink, key string) {
	r.mu.Lock()
	empty := removeSub(r.subs, key, c)
	if empty {
		delete(r.lastEvent, key)
		delete(r.remoteDropped, key)
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
		delete(r.remoteDropped, key)
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
	r.remoteDropped = make(map[string]bool)
	r.mu.Unlock()

	// Unwinds any in-flight FetchEvents inside sendHistoryToClient.
	r.baseCancel()

	if conn != nil {
		conn.Close()
	}

	// Bounded by the 5s request timeout in the worst case.
	r.wg.Wait()
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
		// Close() may have raced the dial; never hand out a conn on a closed relay.
		if r.closed {
			return fmt.Errorf("relay closed")
		}
		if r.conn != nil {
			return nil
		}
		return fmt.Errorf("connection attempt failed")
	}
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
	// Same SSRF surface as doRequest: the dashboard token rides this dial, so
	// never dial an unvalidated peer URL (#1548).
	if r.node.urlErr != nil {
		return fmt.Errorf("relay %s: refusing to dial unvalidated peer URL: %w", r.node.ID, r.node.urlErr)
	}
	// Prefix match (not Replace) so a path containing "http://" is not mangled;
	// https must be tested first.
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
		// Pin the TLS floor for node-to-node wss://.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", r.node.ID, err)
	}

	if err := conn.WriteJSON(ClientMsg{Type: "auth", Token: r.node.Token}); err != nil {
		conn.Close()
		return fmt.Errorf("auth write %s: %w", r.node.ID, err)
	}
	var resp ServerMsg
	// A failed SetReadDeadline (half-closed conn) would leave ReadJSON open-ended.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		conn.Close()
		return fmt.Errorf("auth set read deadline %s: %w", r.node.ID, err)
	}
	if err := conn.ReadJSON(&resp); err != nil {
		conn.Close()
		return fmt.Errorf("auth read %s: %w", r.node.ID, err)
	}
	if resp.Type != "auth_ok" {
		conn.Close()
		// resp.Error is remote-supplied and reaches slog via the err wrap.
		return fmt.Errorf("auth failed %s: %s", r.node.ID, osutil.SanitizeForLog(resp.Error, 256))
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return fmt.Errorf("auth clear read deadline %s: %w", r.node.ID, err)
	}

	// Read deadline + pong handler detect silent disconnects (NAT timeout).
	if err := conn.SetReadDeadline(time.Now().Add(relayReadTimeout)); err != nil {
		conn.Close()
		return fmt.Errorf("set live read deadline %s: %w", r.node.ID, err)
	}
	conn.SetPongHandler(func(string) error {
		// A failure here surfaces on the next ReadMessage; nothing to do.
		_ = conn.SetReadDeadline(time.Now().Add(relayReadTimeout))
		return nil
	})

	r.mu.Lock()
	if r.conn != nil || r.closed {
		// Lost the race to another dialer, or Close() ran mid-dial.
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
		// Singleflight: the CAS blocks a second reconnect enqueued by this same
		// defer while the primary reconnect() is still resubscribing.
		if r.reconnecting.CompareAndSwap(false, true) {
			go func() {
				defer r.reconnecting.Store(false)
				r.reconnect()
			}()
		}
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
		// A failed SetReadDeadline would let the next ReadMessage block forever;
		// fail the loop so the defer reconnects.
		if err := conn.SetReadDeadline(time.Now().Add(relayReadTimeout)); err != nil {
			return
		}

		r.forwardEvent(data)
	}
}

// forwardEvent parses the routing header, updates lastEvent under r.mu,
// snapshots the subscribers and fans out SendRaw OUTSIDE the lock — SendRaw
// has no non-blocking contract, so a slow sink must not stall every
// Subscribe/Unsubscribe/Close (#2187).
func (r *wsRelay) forwardEvent(data []byte) {
	// Only the routing fields are parsed; the node field is injected into the
	// raw bytes instead of a full unmarshal+remarshal.
	var header struct {
		Type   string `json:"type"`
		Key    string `json:"key"`
		Reason string `json:"reason"`
		Event  struct {
			Time int64 `json:"time"`
		} `json:"event"`
	}
	if json.Unmarshal(data, &header) != nil {
		return
	}

	tagged := injectNodeField(data, r.nodeField)
	r.mu.Lock()
	if header.Type == "event" && header.Event.Time > r.lastEvent[header.Key] {
		r.lastEvent[header.Key] = header.Event.Time
	}
	subs := r.subs[header.Key]
	// The remote dropped OUR subscription for this key; mark it for rebuild,
	// but only while a local subscriber still holds the key (no map growth).
	if header.Type == "session_state" && header.Reason == "subscription_timeout" && len(subs) > 0 {
		r.remoteDropped[header.Key] = true
	}
	snapPtr := subSnapPool.Get().(*[]EventSink)
	clients := *snapPtr
	if cap(clients) < len(subs) {
		clients = make([]EventSink, len(subs))
	} else {
		clients = clients[:len(subs)]
	}
	copy(clients, subs)
	r.mu.Unlock()

	for _, c := range clients {
		c.SendRaw(tagged)
	}

	// Clear pointers so disconnected sinks are not pinned by the pool.
	for i := range clients {
		clients[i] = nil
	}
	// Never pool an arbitrarily large backing array after a subscriber spike.
	if cap(clients) <= 256 {
		*snapPtr = clients[:0]
		subSnapPool.Put(snapPtr)
	}
}

// nodeKeyProbe is package-level to avoid a per-event allocation on the hot path.
var nodeKeyProbe = []byte(`"node":`)

// injectNodeField inserts the pre-computed "node":"id", field into raw JSON
// without a decode/encode; a message that already carries a "node" key is
// returned as-is (duplicate keys resolve parser-defined). The result is NOT
// pooled: SendRaw enqueues it by reference into every subscriber's send channel,
// so there is no safe point to reuse the buffer.
func injectNodeField(data, nodeField []byte) []byte {
	if len(data) == 0 || data[0] != '{' {
		return data
	}
	// Whole-payload scan (a short peek window missed keys after a long
	// session key and double-injected); `"node":` with colon avoids matching a
	// value.
	if bytes.Contains(data, nodeKeyProbe) {
		return data
	}
	// "{}": nodeField ends with ',' which would yield {"node":"id",}.
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
		// Jitter so N relays reconnecting after a primary restart do not hit
		// the listener at identical offsets.
		t := time.NewTimer(osutil.JitterBackoff(backoff))
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

		// Resubscribe every active key from its last-seen timestamp.
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
		// Every held key is re-subscribed below, so no remote drop is outstanding.
		clear(r.remoteDropped)
		r.mu.Unlock()

		for _, e := range resubscribes {
			r.writeJSON(ClientMsg{Type: "subscribe", Key: e.key, After: e.after})
		}
		// writeJSON silently returns once r.closed, so a Close() racing the
		// loop above would otherwise be reported as a false "reconnected".
		r.mu.Lock()
		stillOpen := !r.closed
		connAlive := r.conn != nil
		r.mu.Unlock()
		if !stillOpen {
			slog.Warn("relay reconnect aborted by close", "node", r.node.ID, "keys", len(resubscribes))
			return
		}
		// If the new socket died during the resubscribe window its readLoop
		// could not enqueue a reconnect (this goroutine holds the flag);
		// returning would leave the relay permanently disconnected, so redial.
		if !connAlive {
			slog.Warn("relay reconnect: new conn died during resubscribe, retrying", "node", r.node.ID, "keys", len(resubscribes))
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		slog.Info("relay reconnected", "node", r.node.ID, "keys", len(resubscribes))
		return
	}
}

// sendHistoryToClient serves the second-subscriber path; the caller does
// wg.Add(1) and this function owns the matching Done.
func (r *wsRelay) sendHistoryToClient(c EventSink, key string, after int64) {
	defer r.wg.Done()

	c.SendJSON(ServerMsg{Type: "subscribed", Key: key, Node: r.node.ID})

	// Derived from baseCtx so Close() cancels every in-flight fetch.
	ctx, cancel := context.WithTimeout(r.baseCtx, 5*time.Second)
	defer cancel()

	entries, err := r.node.FetchEvents(ctx, key, after)
	if err != nil {
		slog.Warn("relay fetch history", "node", r.node.ID, "key", key, "err", err)
		return
	}
	if len(entries) > 0 {
		c.SendJSON(ServerMsg{Type: "history", Key: key, Node: r.node.ID, Events: entries, Initial: true})
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
	// A failed SetWriteDeadline means a half-closed conn: close to reconnect
	// rather than let WriteJSON block until TCP keepalive expires.
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		slog.Warn("relay set write deadline failed, closing connection for reconnect", "node", r.node.ID, "err", err)
		conn.Close()
		return
	}
	if err := conn.WriteJSON(v); err != nil {
		slog.Warn("relay write failed, closing connection for reconnect", "node", r.node.ID, "err", err)
		conn.Close() // triggers readLoop exit → automatic reconnect
	}
}
