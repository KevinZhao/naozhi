package node

import (
	"encoding/json"

	"github.com/naozhi/naozhi/internal/cli"
)

// ServerMsg is a message sent from the server to the WebSocket client.
type ServerMsg struct {
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

// ClientMsg is a message sent from the WebSocket client.
type ClientMsg struct {
	Type      string `json:"type"`                // auth, subscribe, unsubscribe, send, interrupt, ping
	Token     string `json:"token,omitempty"`     // auth token
	Key       string `json:"key,omitempty"`       // session key
	Text      string `json:"text,omitempty"`      // message text (send)
	ID        string `json:"id,omitempty"`        // client-generated correlation ID
	After     int64  `json:"after,omitempty"`     // unix ms timestamp for subscribe history
	Node      string `json:"node,omitempty"`      // target node (empty = local)
	Workspace string `json:"workspace,omitempty"` // workspace override for new sessions
	ResumeID  string `json:"resume_id,omitempty"` // session ID to resume (recent sessions)
}

// ReverseMsg is the framing message for the reverse-connect WebSocket protocol.
// It is used for both primary→node requests and node→primary responses/events.
type ReverseMsg struct {
	Type        string          `json:"type"`
	NodeID      string          `json:"node_id,omitempty"`
	Token       string          `json:"token,omitempty"`
	DisplayName string          `json:"display_name,omitempty"`
	Hostname    string          `json:"hostname,omitempty"`
	ReqID       string          `json:"req_id,omitempty"`
	Method      string          `json:"method,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	Key         string          `json:"key,omitempty"`
	After       int64           `json:"after,omitempty"`
	Event       *cli.EventEntry `json:"event,omitempty"`
	State       string          `json:"state,omitempty"`
}
