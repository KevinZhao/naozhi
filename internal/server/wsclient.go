package server

import (
	"encoding/json"
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
	wsMaxMessageSize = 262144 // 256KB — code review payloads can exceed 8KB
	wsWriteWait      = 10 * time.Second
	wsPongWait       = 60 * time.Second
	wsPingPeriod     = (wsPongWait * 9) / 10
	wsAuthTimeout    = 5 * time.Second

	// maxWSSendTextBytes bounds a single "send" msg.Text payload. See
	// handleSend for the rationale; summary: wsMaxMessageSize bounds the
	// JSON frame but not the individual text field, and dispatch-queue
	// coalescing can multiply N queued entries into a single CLI stdin
	// write. 64 KB is ~8× the IM-side cap and covers code/stack-trace
	// paste workflows. R59-SEC-H1.
	maxWSSendTextBytes = 64 * 1024
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
	done             chan struct{}
	doneOnce         sync.Once
	dropped          atomic.Int64 // messages dropped due to full send buffer
	uploadOwner      string       // upload-store owner key derived from auth cookie (or IP in no-token mode)
}

func (c *wsClient) closeDone() {
	c.doneOnce.Do(func() { close(c.done) })
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
			slog.Error("panic in ws readPump (recovered)",
				"remote", c.remoteIP, "panic", r, "stack", string(debug.Stack()))
		}
		c.closeDone()
		c.hub.unregister(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(wsMaxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		if c.authenticated.Load() {
			c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		}
		return nil
	})

	if !c.authenticated.Load() {
		c.conn.SetReadDeadline(time.Now().Add(wsAuthTimeout))
	}

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
			// Reuse sendLimiter so authenticated clients can't spin a flood
			// of pings that each trigger json.Marshal + channel send — the
			// work is small per message but easy to amplify without a cap.
			// 5/s burst matches the existing send budget.
			if c.authenticated.Load() && !c.sendLimiter.Allow() {
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
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-c.done:
			return
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
