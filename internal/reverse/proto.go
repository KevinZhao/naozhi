package reverse

import (
	"encoding/json"

	"github.com/naozhi/naozhi/internal/cli"
)

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
