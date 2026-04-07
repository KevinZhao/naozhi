package server

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

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
	conn          *websocket.Conn
	send          chan []byte
	hub           *Hub
	authenticated atomic.Bool
	subscriptions map[string]func() // key -> unsubscribe function
	done          chan struct{}
	doneOnce      sync.Once
	dropped       atomic.Int64 // messages dropped due to full send buffer
}

func (c *wsClient) closeDone() {
	c.doneOnce.Do(func() { close(c.done) })
}

func (c *wsClient) SendJSON(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
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
		// the hub mutex when broadcasting to slow clients.
		c.dropped.Add(1)
	}
}

func (c *wsClient) readPump() {
	defer func() {
		c.closeDone()
		c.hub.unregister(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(wsMaxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
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
			c.hub.handleSend(c, msg)
		case "interrupt":
			if !c.authenticated.Load() {
				c.SendJSON(node.ServerMsg{Type: "error", Error: "not authenticated"})
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
