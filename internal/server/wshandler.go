package server

import (
	"github.com/naozhi/naozhi/internal/cli"
)

// wsClientMsg is a message sent from the WebSocket client.
type wsClientMsg struct {
	Type  string `json:"type"`            // auth, subscribe, unsubscribe, send, ping
	Token string `json:"token,omitempty"` // auth token
	Key   string `json:"key,omitempty"`   // session key
	Text  string `json:"text,omitempty"`  // message text (send)
	ID    string `json:"id,omitempty"`    // client-generated correlation ID
	After int64  `json:"after,omitempty"` // unix ms timestamp for subscribe history
	Node  string `json:"node,omitempty"`  // target node (empty = local)
}

// wsServerMsg is a message sent from the server to the WebSocket client.
type wsServerMsg struct {
	Type   string           `json:"type"`             // auth_ok, auth_fail, subscribed, unsubscribed, history, event, send_ack, pong, error
	Key    string           `json:"key,omitempty"`    // session key
	Event  *cli.EventEntry  `json:"event,omitempty"`  // single event (push)
	Events []cli.EventEntry `json:"events,omitempty"` // event batch (history)
	ID     string           `json:"id,omitempty"`     // correlation ID from client
	Status string           `json:"status,omitempty"` // ack status: accepted, busy, error
	State  string           `json:"state,omitempty"`  // session state
	Reason string           `json:"reason,omitempty"` // additional context
	Error  string           `json:"error,omitempty"`  // error message
	Node   string           `json:"node,omitempty"`   // source node
}
