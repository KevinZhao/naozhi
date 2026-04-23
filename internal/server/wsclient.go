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
	// Reuse the HTTP writeJSON encoder pool: ws control messages (auth_ok,
	// subscribed, history, error, pong, send_ack, interrupt_ack) are on the
	// hot path — 10 active clients produce 40-160 ctrl msg/s — and each
	// json.Marshal allocates a fresh encodeState + result []byte. SendRaw
	// buffers the bytes onto the send channel, so we must copy out of the
	// pooled buffer before returning it.
	e := getJSONEnc()
	defer putJSONEnc(e)
	if err := e.enc.Encode(v); err != nil {
		slog.Debug("ws SendJSON encode", "err", err)
		return
	}
	// Encoder appends a trailing newline; strip it because WS clients expect
	// a bare JSON message (matches the json.Marshal output).
	raw := e.buf.Bytes()
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		raw = raw[:n-1]
	}
	// Copy bytes out of the pool buffer — SendRaw hands the slice to the
	// send channel goroutine which may outlive putJSONEnc.
	data := make([]byte, len(raw))
	copy(data, raw)
	c.SendRaw(data)
}

// SendRaw sends pre-marshalled bytes to the client's send channel (non-blocking).
func (c *wsClient) SendRaw(data []byte) {
	select {
	case c.send <- data:
	case <-c.done:
	default:
		// Drop message if client buffer is full to prevent deadlocking
		// the hub mutex when broadcasting to slow clients.
		c.dropped.Add(1)
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
			c.SendJSON(node.ServerMsg{Type: "pong"})
		}
	}
}

func (c *wsClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
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
