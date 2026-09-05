package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/wsproto"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/node"
)

const (
	// wsMaxMessageSize caps the whole JSON frame the reader will accept.
	// Gorilla allocates a buffer up to this size per ReadMessage, so resident
	// memory is connCount × wsMaxMessageSize. 1.5 MB covers maxWSSendTextBytes
	// plus file_ids and framing with headroom for JSON escape expansion.
	wsMaxMessageSize = 1536 * 1024
	// wsPreAuthMessageSize tightens the read budget while unauthenticated: the
	// only legal message then is `auth` (token ≤ ~256 bytes). Lifted to
	// wsMaxMessageSize once auth succeeds; otherwise a flood of unauth
	// connections could pin ~750 MB of 1.5 MB read buffers at the 500-conn cap.
	wsPreAuthMessageSize = 512
	wsWriteWait          = 10 * time.Second
	wsPongWait           = 60 * time.Second
	wsPingPeriod         = (wsPongWait * 9) / 10
	wsAuthTimeout        = 5 * time.Second

	// maxWSSendTextBytes bounds a single "send" msg.Text payload (see
	// handleSend: the frame cap does not bound the field, and coalescing
	// multiplies queued entries into one CLI stdin write). 1 MB ≈ 250k English
	// tokens / ~330k CJK chars — under the model budget with stdin headroom.
	maxWSSendTextBytes = 1024 * 1024

	// subGenRetentionNanos is how long a wsClient.subGen[key] entry must be
	// kept past its last unsubscribe. resubscribeEvents parks up to 12 × 5s =
	// 60s checking subGen[key] == gen; deleting earlier would let a fresh
	// subscribe's gen=1 match a stale loop's remembered gen and silently resume
	// on the wrong ManagedSession. 75s leaves a 15s buffer past that window.
	subGenRetentionNanos = int64(75 * time.Second)
	// subGenSweepMinIntervalNanos rate-limits opportunistic sweeps of
	// subGenReleaseAt so a flappy client does not turn each subscribe /
	// unsubscribe into an O(map) scan under h.mu; well under the 75s retention.
	subGenSweepMinIntervalNanos = int64(30 * time.Second)
	// subGenHighWaterMark forces an immediate sweep regardless of the throttle,
	// bounding map growth on long-lived clients that flip many panels.
	subGenHighWaterMark = 200

	// wsDropThreshold: past this many cumulative drops the connection is closed
	// so the browser reconnects and resyncs via a fresh `subscribe`. 64 ≈ 1 min
	// of 1Hz updates — only a permanently-slow client hits it.
	wsDropThreshold = 64
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
	// subGenReleaseAt: earliest unix-nano deadline after which subGen[key] may
	// be deleted. Entries cannot be deleted at unsubscribe time: a stale
	// eventPushLoop may still be parked in resubscribeEvents' 60s wait and
	// would resume if a fresh subscribe reset the generation to a value it
	// remembers. nil map == empty; populated lazily on first unsubscribe.
	subGenReleaseAt map[string]int64
	// subGenLastSweepNs: unix-nano of the last sweep; sweeps run at most once
	// per subGenSweepMinIntervalNanos from handleSubscribe / handleUnsubscribe.
	subGenLastSweepNs int64
	done              chan struct{}
	doneOnce          sync.Once
	dropped           atomic.Int64 // messages dropped due to full send buffer
	// uploadOwner is the upload-store owner key (auth cookie, or IP in no-token
	// mode). Written by readPump's handleAuth and read by writePump's unregister
	// path (releaseOwnerSlot) and readPump's send path, hence the atomic
	// pointer (#1776). nil reads as "" via uploadOwnerKey.
	uploadOwner atomic.Pointer[string]
}

// uploadOwnerKey returns the current upload-store owner key, "" when unset.
func (c *wsClient) uploadOwnerKey() string {
	if p := c.uploadOwner.Load(); p != nil {
		return *p
	}
	return ""
}

// setUploadOwner stores the upload-store owner key.
func (c *wsClient) setUploadOwner(owner string) {
	c.uploadOwner.Store(&owner)
}

func (c *wsClient) closeDone() {
	c.doneOnce.Do(func() { close(c.done) })
}

// markSubGenReleasable flags a key for delayed reclamation. Callers MUST hold
// Hub.mu (which serialises c.subscriptions / c.subGen). The entry is NOT
// deleted here; sweepSubGenExpiredLocked reclaims it after the retention
// window, once any stale resubscribeEvents loop has certainly exited.
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
	if len(c.subGenReleaseAt) == 0 {
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
	// json.Marshal returns a fresh []byte handed straight to SendRaw. Pooling a
	// buffer here does not pay: the send-channel hand-off needs the bytes to
	// outlive the call (forcing a make+copy), and stdlib already pools
	// encodeState. Fan-out paths use marshalPooled once per event instead.
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
		// Drop when the buffer is full so a slow client cannot block the hub
		// mutex during broadcast. Per-client and hub-wide counters both bump so
		// /health reports totals without scanning clients. c.hub is nil in
		// unit tests that build a wsClient without a Hub (#1396).
		n := c.dropped.Add(1)
		if c.hub != nil {
			c.hub.droppedTotal.Add(1)
		}
		// A permanently-slow client silently falling behind is worse than a
		// forced reconnect: past wsDropThreshold close the connection so the
		// browser resyncs via a fresh subscribe. doneOnce collapses concurrent
		// trips to a single close.
		if n >= wsDropThreshold {
			c.doneOnce.Do(func() {
				slog.Warn("slow client closed; will reconnect",
					"ip", c.remoteIP, "dropped", n)
				close(c.done)
			})
		}
	}
}

func (c *wsClient) readPump() {
	defer func() {
		if r := recover(); r != nil {
			// Bump the panic counter before logging so observers see the rate
			// even when stack output is truncated.
			serverMetrics.PanicRecovered()
			// Cause at Error, verbose stack at Debug — stack frames would ship
			// internal paths and function names to journald / log aggregators.
			slog.Error("panic in ws readPump (recovered)",
				"remote", c.remoteIP, "panic", fmt.Sprintf("%v", r))
			slog.Debug("panic in ws readPump: stack",
				"remote", c.remoteIP, "stack", string(debug.Stack()))
		}
		c.closeDone()
		c.hub.unregister(c)
		c.conn.Close()
	}()

	// Pre-auth the only legal frame is `auth`; granting the full 1.5 MB buffer
	// to every upgraded socket would let a flood of unauth connections pin
	// ~750 MB before wsAuthTimeout reaps them. Lifted on auth success below.
	if c.authenticated.Load() {
		c.conn.SetReadLimit(wsMaxMessageSize)
	} else {
		c.conn.SetReadLimit(wsPreAuthMessageSize)
	}
	// Unauthenticated connections get the shorter auth window; authenticated
	// ones the full pong window so the PongHandler keeps them alive. A failed
	// SetReadDeadline means ReadMessage could block forever on a half-closed
	// connection, so return (matching writePump) rather than ignore it.
	var deadlineErr error
	if c.authenticated.Load() {
		deadlineErr = c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	} else {
		deadlineErr = c.conn.SetReadDeadline(time.Now().Add(wsAuthTimeout))
	}
	if deadlineErr != nil {
		return
	}
	c.conn.SetPongHandler(func(string) error {
		if c.authenticated.Load() {
			// Pong handler errors propagate to ReadMessage as a hard error,
			// terminating the loop on the next iteration. No need to bail
			// from the handler itself.
			_ = c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
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
		case string(wsproto.TypeAuth):
			if c.authAttempts.Add(1) > 3 {
				return // closes connection via defer
			}
			c.hub.handleAuth(c, msg)
			if c.authenticated.Load() {
				// Lift the pre-auth read limit now that the peer may send
				// legitimate `send` payloads up to wsMaxMessageSize.
				c.conn.SetReadLimit(wsMaxMessageSize)
				if err := c.conn.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
					return
				}
			}
		case string(wsproto.TypeSubscribe):
			if !c.authenticated.Load() {
				// Pre-marshalled frame avoids reflect.Marshal on the rejection
				// path; byte-equality is locked by TestWSPreMarshalledFrames.
				c.SendRaw([]byte(wsproto.RawErrNotAuth))
				continue
			}
			c.hub.handleSubscribe(c, msg)
		case string(wsproto.TypeUnsubscribe):
			if !c.authenticated.Load() {
				continue
			}
			c.hub.handleUnsubscribe(c, msg)
		case string(wsproto.TypeSend):
			if !c.authenticated.Load() {
				c.SendRaw([]byte(wsproto.RawErrNotAuth))
				continue
			}
			if !c.sendLimiter.Allow() {
				c.SendRaw([]byte(wsproto.RawErrRateLimited))
				continue
			}
			// Per-user (uploadOwner) ceiling so N tabs cannot multiply the burst
			// budget by N; consulted only after the per-conn limiter admits the
			// call, preserving single-tab burst semantics (#888).
			if !c.hub.allowSendForOwner(c.uploadOwnerKey()) {
				c.SendRaw([]byte(wsproto.RawErrRateLimited))
				continue
			}
			c.hub.handleSend(c, msg)
		case string(wsproto.TypeInterrupt):
			if !c.authenticated.Load() {
				c.SendRaw([]byte(wsproto.RawErrNotAuth))
				continue
			}
			if !c.interruptLimiter.Allow() {
				c.SendRaw([]byte(wsproto.RawErrRateLimited))
				continue
			}
			c.hub.handleInterrupt(c, msg)
		case string(wsproto.TypePing):
			// Reuse sendLimiter so a ping flood cannot amplify channel sends;
			// applied unconditionally so unauthenticated connections also pay
			// before wsAuthTimeout evicts them.
			if !c.sendLimiter.Allow() {
				continue
			}
			c.SendRaw([]byte(wsproto.RawPong))
		case string(wsproto.TypeAgentSubscribe):
			if !c.authenticated.Load() {
				c.SendRaw([]byte(wsproto.RawErrNotAuth))
				continue
			}
			// Reuse sendLimiter's budget — a client cannot spin subscribe
			// loops to pin tailers and DoS the 50-tailer cap.
			if !c.sendLimiter.Allow() {
				continue
			}
			c.hub.handleAgentSubscribe(c, msg)
		case string(wsproto.TypeAgentUnsubscribe):
			if !c.authenticated.Load() {
				continue
			}
			c.hub.handleAgentUnsubscribe(c, msg)
		}
	}
}

func (c *wsClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		if r := recover(); r != nil {
			// Mirror readPump: counter first, cause at Error, stack at Debug.
			serverMetrics.PanicRecovered()
			slog.Error("panic in ws writePump (recovered)",
				"remote", c.remoteIP, "panic", fmt.Sprintf("%v", r))
			slog.Debug("panic in ws writePump: stack",
				"remote", c.remoteIP, "stack", string(debug.Stack()))
		}
		ticker.Stop()
		// When writePump exits first (e.g. RST on a ping write while readPump is
		// blocked in ReadMessage): mark done so broadcasts stop queueing,
		// unregister so new subscribes can't target the dying conn, then close
		// the conn last so readPump unblocks (closeDone/unregister are idempotent).
		c.closeDone()
		c.hub.unregister(c)
		c.conn.Close()
	}()

	// Rolling deadline: refresh only when the remaining slack drops below half
	// of wsWriteWait, saving two vDSO calls per event on high-throughput
	// clients. The deadline is always between wsWriteWait/2 and wsWriteWait
	// ahead, so a stalled writer is still killed within ~wsWriteWait.
	const refreshInterval = wsWriteWait / 2
	var nextRefreshAt time.Time
	refreshDeadline := func() error {
		now := time.Now()
		if !now.Before(nextRefreshAt) {
			if err := c.conn.SetWriteDeadline(now.Add(wsWriteWait)); err != nil {
				return err
			}
			nextRefreshAt = now.Add(refreshInterval)
		}
		return nil
	}

	for {
		select {
		case message := <-c.send:
			// A failed SetWriteDeadline (conn closed) must return so the defer
			// unregisters; without a deadline WriteMessage could block on a
			// half-closed socket until TCP keepalive expires.
			if err := refreshDeadline(); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-c.done:
			return
		case <-ticker.C:
			if err := refreshDeadline(); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
