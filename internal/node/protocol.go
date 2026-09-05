package node

import (
	"encoding/json"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// ServerMsg is the legacy flat union of every browser WS frame. DECODE-ONLY
// since #2535: construction goes through internal/wsproto's per-type New*
// frames (wsproto's literal-ban test rejects new ServerMsg{Type: …}
// literals); this type survives for the relay's client-side decode and for
// tests that unmarshal frames. Removal comes with the {type,payload}
// envelope phase.
type ServerMsg struct {
	Type   string                `json:"type"`             // auth_ok, auth_fail, subscribed, unsubscribed, history, event, send_ack, send_error, pong, error, agent_event, agent_meta, agent_done, agent_subscribe_rejected
	Key    string                `json:"key,omitempty"`    // session key
	Event  *clievent.EventEntry  `json:"event,omitempty"`  // single event (push); also reused for agent_event body
	Events []clievent.EventEntry `json:"events,omitempty"` // event batch (history)
	ID     string                `json:"id,omitempty"`     // correlation ID from client
	Status string                `json:"status,omitempty"` // ack status: accepted, busy, error; also task_done status
	State  string                `json:"state,omitempty"`  // session state
	Reason string                `json:"reason,omitempty"` // additional context
	Error  string                `json:"error,omitempty"`  // error message
	Node   string                `json:"node,omitempty"`   // source node
	// RetryAfter (seconds, advisory) is set only on rate-limit auth_fail,
	// mirroring the HTTP Retry-After header on /api/auth/login 429.
	RetryAfter int `json:"retry_after,omitempty"`

	// Agent-team fields (RFC v4 agent-team-ui §3.5.2), all omitempty.
	// TODO(docs/TODO.md): agent_event has no per-message seq; the dashboard
	// de-dups replay/live overlap via (time, type, tool_use_id).
	TaskID    string          `json:"task_id,omitempty"`
	AgentMeta *AgentMetaPatch `json:"meta,omitempty"`

	// HasMore, on the initial "history" frame, says whether events older than
	// the slice exist, replacing the client's length heuristic. *bool so an
	// explicit false still serializes; nil (older servers, non-initial frames)
	// means "fall back to the heuristic".
	HasMore *bool `json:"has_more,omitempty"`

	// Initial marks a "history" frame as the opening page the dashboard renders
	// by replacing the pane; every other history frame is an incremental
	// append and MUST leave it false. Arrival order cannot be used: the first
	// subscriber's history fetch and the remote `subscribed` ack race, as can a
	// superseded subscription's in-flight backfill. omitempty keeps incremental
	// frames byte-identical (poolable, shared across a multi-tab fan-out).
	Initial bool `json:"initial,omitempty"`
}

// AgentMetaPatch aliases the wsproto definition — the browser protocol owns
// the shape (#2535); this name survives for the decode-side consumers of
// ServerMsg.
type AgentMetaPatch = wsproto.AgentMetaPatch

// ClientMsg is a message sent from the WebSocket client.
type ClientMsg struct {
	Type      string `json:"type"`                // auth, subscribe, unsubscribe, send, interrupt, ping, agent_subscribe, agent_unsubscribe
	Token     string `json:"token,omitempty"`     // auth token
	Key       string `json:"key,omitempty"`       // session key
	Text      string `json:"text,omitempty"`      // message text (send)
	ID        string `json:"id,omitempty"`        // client-generated correlation ID
	After     int64  `json:"after,omitempty"`     // unix ms watermark for subscribe history; the after-ms itself is re-admitted (#2432), client dedups by uuid
	Before    int64  `json:"before,omitempty"`    // unix ms timestamp; history page < Before (pagination)
	Limit     int    `json:"limit,omitempty"`     // max events to return from initial / paginated history
	Node      string `json:"node,omitempty"`      // target node (empty = local)
	Workspace string `json:"workspace,omitempty"` // workspace override for new sessions
	ResumeID  string `json:"resume_id,omitempty"` // session ID to resume (recent sessions)
	Backend   string `json:"backend,omitempty"`   // backend ID picked by dashboard for new sessions
	// AccessProfile is the profile picked for new LOCAL sessions; remote
	// dispatch of a non-default profile is refused (the overlay is host-local).
	AccessProfile string   `json:"access_profile,omitempty"`
	FileIDs       []string `json:"file_ids,omitempty"` // pre-uploaded image IDs from /api/sessions/upload
	// TaskID is the agent-team subscribe target (agent_subscribe / agent_unsubscribe).
	TaskID string `json:"task_id,omitempty"`
}

// ReverseMsg frames the reverse-connect WebSocket protocol in both
// directions. ProtocolVersion / Capabilities appear on the register handshake
// only; a peer omitting them is version 1 with no caps. The server MUST NOT
// fail-close on unknown capability strings so mixed versions interoperate.
type ReverseMsg struct {
	Type string `json:"type"`
	// ProtocolVersion is the wire version (implicit 1); bumped on breaking
	// framing changes only, additive fields stay omitempty.
	ProtocolVersion int `json:"protocol_version,omitempty"`
	// Capabilities advertises optional feature tags ("acp", "askuser", …);
	// unknown tags are ignored, not rejected.
	Capabilities []string              `json:"capabilities,omitempty"`
	NodeID       string                `json:"node_id,omitempty"`
	Token        string                `json:"token,omitempty"`
	DisplayName  string                `json:"display_name,omitempty"`
	Hostname     string                `json:"hostname,omitempty"`
	ReqID        string                `json:"req_id,omitempty"`
	Method       string                `json:"method,omitempty"`
	Params       json.RawMessage       `json:"params,omitempty"`
	Result       json.RawMessage       `json:"result,omitempty"`
	Error        string                `json:"error,omitempty"`
	Key          string                `json:"key,omitempty"`
	After        int64                 `json:"after,omitempty"`
	Event        *clievent.EventEntry  `json:"event,omitempty"`
	Events       []clievent.EventEntry `json:"events,omitempty"`
	State        string                `json:"state,omitempty"`
	Reason       string                `json:"reason,omitempty"`
}
