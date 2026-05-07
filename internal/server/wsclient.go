package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/node"
)

const (
	// wsMaxMessageSize caps the whole JSON frame the reader will accept.
	// Gorilla WebSocket allocates a buffer up to this size per ReadMessage,
	// so the worst-case resident memory is connCount × wsMaxMessageSize.
	// Budget: maxWSSendTextBytes (1 MB text, JSON-escaped worst case ≈
	// 1 MB × 6 bytes for all-`"`) + 10 × 80-byte file_id hex + small
	// framing (key, workspace, backend, node, id, type) ≤ ~1.15 MB for
	// non-pathological payloads. 1.5 MB leaves headroom for JSON escape
	// expansion without paying 2 MB per connection. R68-GO-H1.
	wsMaxMessageSize = 1536 * 1024
	wsWriteWait      = 10 * time.Second
	wsPongWait       = 60 * time.Second
	wsPingPeriod     = (wsPongWait * 9) / 10
	wsAuthTimeout    = 5 * time.Second

	// maxWSSendTextBytes bounds a single "send" msg.Text payload. See
	// handleSend for the rationale; summary: wsMaxMessageSize bounds the
	// JSON frame but not the individual text field, and dispatch-queue
	// coalescing can multiply N queued entries into a single CLI stdin
	// write. 1 MB matches the ~1M-token context window of modern models
	// (~4 bytes/token UTF-8 lower bound, so 1 MB ≈ 250k tokens of English
	// or ~330k CJK characters — comfortably under the model budget while
	// still leaving CLI-stdin headroom with the coalesce/MaxDepth caps.
	// R59-SEC-H1.
	maxWSSendTextBytes = 1024 * 1024

	// subGenRetentionNanos is how long a wsClient.subGen[key] entry must be
	// kept alive past its last unsubscribe before it can be reclaimed. The
	// stale-goroutine hazard R163 warned about lives inside resubscribeEvents
	// (see its godoc): that loop runs 12 iterations of 5s timer waits = 60s
	// max, checking subGen[key] == gen each iteration to detect takeover.
	// Deleting subGen[key] while a stale loop is still parked would let a
	// fresh subscribe's gen=1 match the stale loop's remembered gen=1 and
	// silently resume on the wrong ManagedSession. 75s retention puts a
	// comfortable 15s buffer past the worst-case park window. R175-P2.
	subGenRetentionNanos = int64(75 * time.Second)
	// subGenSweepMinIntervalNanos rate-limits opportunistic scans of
	// subGenReleaseAt so a flurry of subscribe/unsubscribe messages from a
	// flappy client does not turn each handler into an O(map) scan under
	// h.mu. 30s is well under the 75s retention window so expired entries
	// are still reclaimed promptly.
	subGenSweepMinIntervalNanos = int64(30 * time.Second)
	// subGenHighWaterMark is the entry count above which we force an
	// immediate sweep regardless of the last-sweep throttle. This bounds
	// worst-case map growth on long-lived clients that open/close many
	// session panels between the natural 30s sweep ticks.
	subGenHighWaterMark = 200
)

type wsClient struct {
	conn             *websocket.Conn
	send             chan []byte
	hub              *Hub
	remoteIP         string // for rate limiting
	authenticated    atomic.Bool
	authAttempts     atomic.Int32
	sendLimiter      *rate.Limiter     // per-connection rate limit on "send" messages
	interruptLimiter *rate.Limiter     // per-connection rate limit on "interrupt" messages (separate from send)
	subscriptions    map[string]func() // key -> unsubscribe function
	subGen           map[string]uint64 // key -> subscription generation (detects resubscribe race)
	// subGenReleaseAt tracks the earliest wall-clock unix-nano deadline after
	// which a subGen[key] entry may be deleted. R175-P2: long-lived dashboard
	// clients that rapidly open/close many session panels accumulate subGen
	// entries forever because R163's contract forbids deleting them at
	// unsubscribe time (stale eventPushLoop goroutines may still be parked in
	// resubscribeEvents' 60s ticker and would silently resume if a fresh
	// subscribe reset the generation back to a value they remember). The
	// retention window (subGenRetentionNanos) is longer than the longest
	// possible resubscribeEvents wait (12 × 5s = 60s) so stale goroutines
	// are guaranteed to have exited before their subGen entry is reclaimed.
	// A nil map is equivalent to "empty"; the map is lazily populated only
	// when a key is actually unsubscribed.
	subGenReleaseAt map[string]int64
	// subGenLastSweepNs is the unix-nano of the last opportunistic sweep of
	// subGenReleaseAt. Sweeps run at most once per subGenSweepMinIntervalNs
	// from handleSubscribe / handleUnsubscribe so N concurrent clients flapping
	// subscribes do not each trigger an O(map) scan under h.mu.
	subGenLastSweepNs int64
	done              chan struct{}
	doneOnce          sync.Once
	dropped           atomic.Int64 // messages dropped due to full send buffer
	uploadOwner       string       // upload-store owner key derived from auth cookie (or IP in no-token mode)
}

func (c *wsClient) closeDone() {
	c.doneOnce.Do(func() { close(c.done) })
}

// markSubGenReleasable flags a key for delayed reclamation. Callers MUST hold
// Hub.mu (the enclosing mutex that serialises access to c.subscriptions /
// c.subGen). The entry itself is NOT deleted here; sweepSubGenExpiredLocked
// performs the deletion later once the 75s retention window elapses. This
// preserves the R163 stale-goroutine contract: resubscribeEvents keeps
// checking subGen[key] == gen for up to 60s after an unsubscribe, and we
// only reclaim after any such stale goroutine is guaranteed to have exited.
func (c *wsClient) markSubGenReleasable(key string, nowNanos int64) {
	if c.subGenReleaseAt == nil {
		c.subGenReleaseAt = make(map[string]int64)
	}
	c.subGenReleaseAt[key] = nowNanos + subGenRetentionNanos
}

// clearSubGenReleasable cancels a pending reclamation. Callers MUST hold
// Hub.mu. Used when a fresh subscribe arrives for a key that was marked for
// release, so the retention marker does not outlive the now-active subscription.
func (c *wsClient) clearSubGenReleasable(key string) {
	if c.subGenReleaseAt == nil {
		return
	}
	delete(c.subGenReleaseAt, key)
}

// sweepSubGenExpiredLocked reclaims expired subGen entries. Callers MUST hold
// Hub.mu. Throttled by subGenLastSweepNs + subGenSweepMinIntervalNanos unless
// the map has grown past subGenHighWaterMark (in which case a sweep runs
// regardless, to put a hard bound on memory). Returns the number of entries
// reclaimed, for observability in tests.
func (c *wsClient) sweepSubGenExpiredLocked(nowNanos int64) int {
	if c.subGenReleaseAt == nil || len(c.subGenReleaseAt) == 0 {
		return 0
	}
	// Throttle: skip the scan if a recent sweep ran, unless the map has
	// grown past the high-water mark (forces the scan to bound memory).
	if len(c.subGenReleaseAt) < subGenHighWaterMark &&
		nowNanos-c.subGenLastSweepNs < subGenSweepMinIntervalNanos {
		return 0
	}
	c.subGenLastSweepNs = nowNanos
	reclaimed := 0
	for key, releaseAt := range c.subGenReleaseAt {
		if nowNanos < releaseAt {
			continue
		}
		// Final safety check: if the key is still actively subscribed, the
		// marker is stale (bookkeeping bug elsewhere); leave subGen[key] in
		// place and just drop the marker. Otherwise reclaim both.
		if _, active := c.subscriptions[key]; active {
			delete(c.subGenReleaseAt, key)
			continue
		}
		delete(c.subGen, key)
		delete(c.subGenReleaseAt, key)
		reclaimed++
	}
	return reclaimed
}

func (c *wsClient) SendJSON(v any) {
	// json.Marshal returns a fresh []byte we can hand directly to SendRaw
	// (no copy needed; stdlib already pools encodeState internally). The
	// previous encoder-pool path required a make+copy to isolate the send
	// channel from the returned pool buffer, making it strictly more
	// expensive than plain Marshal for this single-producer hot path.
	data, err := json.Marshal(v)
	if err != nil {
		slog.Debug("ws SendJSON encode", "err", err)
		return
	}
	c.SendRaw(data)
}

// SendRaw sends pre-marshalled bytes to the client's send channel (non-blocking).
func (c *wsClient) SendRaw(data []byte) {
	select {
	case c.send <- data:
	case <-c.done:
	default:
		// Drop message if client buffer is full to prevent deadlocking
		// the hub mutex when broadcasting to slow clients. Both per-client
		// and hub-wide counters bump so /health can report totals without
		// scanning the clients map under RLock.
		c.dropped.Add(1)
		c.hub.droppedTotal.Add(1)
	}
}

func (c *wsClient) readPump() {
	defer func() {
		if r := recover(); r != nil {
			// Log the panic cause at Error so operators are alerted, but
			// keep the verbose stack trace at Debug — shipping it to
			// journald / aggregated log stores would broadcast internal
			// file paths and function names, which is not useful after
			// the fact once the panic type is known.
			slog.Error("panic in ws readPump (recovered)",
				"remote", c.remoteIP, "panic", fmt.Sprintf("%v", r))
			slog.Debug("panic in ws readPump: stack",
				"remote", c.remoteIP, "stack", string(debug.Stack()))
		}
		c.closeDone()
		c.hub.unregister(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(wsMaxMessageSize)
	// Unauthenticated connections get the shorter auth window; authenticated
	// ones get the full pong window so the PongHandler keeps them alive.
	// Single write avoids the earlier "set wsPongWait then immediately
	// overwrite with wsAuthTimeout" dead-code pattern.
	if c.authenticated.Load() {
		c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	} else {
		c.conn.SetReadDeadline(time.Now().Add(wsAuthTimeout))
	}
	c.conn.SetPongHandler(func(string) error {
		if c.authenticated.Load() {
			c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		}
		return nil
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}

		var msg node.ClientMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "auth":
			if c.authAttempts.Add(1) > 3 {
				return // closes connection via defer
			}
			c.hub.handleAuth(c, msg)
			if c.authenticated.Load() {
				c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
			}
		case "subscribe":
			if !c.authenticated.Load() {
				c.SendJSON(node.ServerMsg{Type: "error", Error: "not authenticated"})
				continue
			}
			c.hub.handleSubscribe(c, msg)
		case "unsubscribe":
			if !c.authenticated.Load() {
				continue
			}
			c.hub.handleUnsubscribe(c, msg)
		case "send":
			if !c.authenticated.Load() {
				c.SendJSON(node.ServerMsg{Type: "error", Error: "not authenticated"})
				continue
			}
			if !c.sendLimiter.Allow() {
				c.SendJSON(node.ServerMsg{Type: "error", Error: "rate limited"})
				continue
			}
			c.hub.handleSend(c, msg)
		case "interrupt":
			if !c.authenticated.Load() {
				c.SendJSON(node.ServerMsg{Type: "error", Error: "not authenticated"})
				continue
			}
			if !c.interruptLimiter.Allow() {
				c.SendJSON(node.ServerMsg{Type: "error", Error: "rate limited"})
				continue
			}
			c.hub.handleInterrupt(c, msg)
		case "ping":
			// Reuse sendLimiter so *any* client can't spin a flood of pings
			// that each trigger json.Marshal + channel send — the work is
			// small per message but easy to amplify without a cap. Applied
			// unconditionally so unauthenticated connections also pay the
			// budget before the wsAuthTimeout evicts them.
			if !c.sendLimiter.Allow() {
				continue
			}
			c.SendJSON(node.ServerMsg{Type: "pong"})
		}
	}
}

func (c *wsClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		// When writePump exits first (e.g. TCP RST on a ping write while
		// readPump is still blocked in ReadMessage), we must mark the client
		// as done so broadcasts stop queueing, and unregister from the hub so
		// new subscribes can't target this dying conn. Close the underlying
		// connection last so readPump unblocks and its defer can also run
		// (closeDone/unregister are idempotent). Without this, the hub kept
		// a live entry for the zombie until the kernel eventually closed the
		// socket, inflating broadcast fan-out with dead clients.
		c.closeDone()
		c.hub.unregister(c)
		c.conn.Close()
	}()

	for {
		select {
		case message := <-c.send:
			// If SetWriteDeadline fails (conn closed), fall through to return
			// so the defer closes/unregisters. Without a deadline, WriteMessage
			// could block on a half-closed socket until TCP keepalive expires,
			// leaving the hub broadcasting to a zombie client.
			if err := c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
