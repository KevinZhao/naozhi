package node

import (
	"encoding/json"

	"github.com/naozhi/naozhi/internal/cli"
)

// ServerMsg is a message sent from the server to the WebSocket client.
type ServerMsg struct {
	Type   string           `json:"type"`             // auth_ok, auth_fail, subscribed, unsubscribed, history, event, send_ack, pong, error, agent_event, agent_meta, agent_done, agent_subscribe_rejected
	Key    string           `json:"key,omitempty"`    // session key
	Event  *cli.EventEntry  `json:"event,omitempty"`  // single event (push); also reused for agent_event body
	Events []cli.EventEntry `json:"events,omitempty"` // event batch (history)
	ID     string           `json:"id,omitempty"`     // correlation ID from client
	Status string           `json:"status,omitempty"` // ack status: accepted, busy, error; also task_done status
	State  string           `json:"state,omitempty"`  // session state
	Reason string           `json:"reason,omitempty"` // additional context
	Error  string           `json:"error,omitempty"`  // error message
	Node   string           `json:"node,omitempty"`   // source node
	// RetryAfter is advisory: when set on auth_fail rate-limit replies, the
	// client should wait at least this many seconds before retrying. Mirrors
	// the HTTP Retry-After header the /api/auth/login 429 branch emits, so
	// WS auth and HTTP login lockouts surface identical UX affordances.
	// Omitted on non-rate-limit auth_fail (e.g. invalid token) and on all
	// other message types — older clients silently ignore the unknown field.
	RetryAfter int `json:"retry_after,omitempty"`

	// Agent-team fields (RFC v4 agent-team-ui §3.5.2). All omitempty; older
	// clients silently ignore unknown fields and older servers never emit
	// the "agent_*" message types to trigger them.
	//
	// TODO(RFC v4 phase 3, tracked in docs/TODO.md R214-CODE-6): agent_event
	// has no per-message seq; on client reconnect during a buffered-replay +
	// live-push overlap, the dashboard must de-dup via
	// (time, type, tool_use_id) composite key. Consider adding a monotonic
	// seq here if Phase 3 observes duplicates in practice.
	TaskID    string          `json:"task_id,omitempty"`
	AgentMeta *AgentMetaPatch `json:"meta,omitempty"`
}

// AgentMetaPatch carries aggregator-side counters the server_tailer pushes
// out-of-band of the raw event stream. Consumed by the dashboard to refresh
// a banner row's "N calls · 2.1s" without re-rendering the entire agent view.
type AgentMetaPatch struct {
	LastTool   string `json:"last_tool,omitempty"`
	LastDetail string `json:"last_detail,omitempty"`
	ToolUses   int    `json:"tool_uses,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// ClientMsg is a message sent from the WebSocket client.
type ClientMsg struct {
	Type      string   `json:"type"`                // auth, subscribe, unsubscribe, send, interrupt, ping, agent_subscribe, agent_unsubscribe
	Token     string   `json:"token,omitempty"`     // auth token
	Key       string   `json:"key,omitempty"`       // session key
	Text      string   `json:"text,omitempty"`      // message text (send)
	ID        string   `json:"id,omitempty"`        // client-generated correlation ID
	After     int64    `json:"after,omitempty"`     // unix ms timestamp for subscribe history
	Before    int64    `json:"before,omitempty"`    // unix ms timestamp; history page < Before (pagination)
	Limit     int      `json:"limit,omitempty"`     // max events to return from initial / paginated history
	Node      string   `json:"node,omitempty"`      // target node (empty = local)
	Workspace string   `json:"workspace,omitempty"` // workspace override for new sessions
	ResumeID  string   `json:"resume_id,omitempty"` // session ID to resume (recent sessions)
	Backend   string   `json:"backend,omitempty"`   // backend ID picked by dashboard for new sessions
	FileIDs   []string `json:"file_ids,omitempty"`  // pre-uploaded image IDs from /api/sessions/upload
	// Agent-team subscribe target (RFC v4 agent-team-ui §3.5.2). Set on
	// agent_subscribe / agent_unsubscribe only; unused by legacy message
	// types which ignore the extra field.
	TaskID string `json:"task_id,omitempty"`
}

// ReverseMsg is the framing message for the reverse-connect WebSocket protocol.
// It is used for both primary→node requests and node→primary responses/events.
//
// Forward-compat markers (ProtocolVersion / Capabilities) are set on the
// register handshake only. A peer omitting them is treated as version 1 with
// empty capabilities. Server-side MUST NOT fail-close on unknown capability
// strings — consumers intersect advertised caps with their known set and
// gracefully degrade for anything missing. This keeps newer nodes/primaries
// interoperating with older peers without a flag-day upgrade.
type ReverseMsg struct {
	Type string `json:"type"`
	// ProtocolVersion is the reverse-node wire version the sender speaks.
	// Current implicit version is 1. Bumped on breaking framing changes only;
	// additive fields continue to use omitempty without a version bump.
	ProtocolVersion int `json:"protocol_version,omitempty"`
	// Capabilities advertises optional feature tags (e.g. "gemini", "acp",
	// "askuser") on register. Unknown tags are ignored, not rejected.
	Capabilities []string         `json:"capabilities,omitempty"`
	NodeID       string           `json:"node_id,omitempty"`
	Token        string           `json:"token,omitempty"`
	DisplayName  string           `json:"display_name,omitempty"`
	Hostname     string           `json:"hostname,omitempty"`
	ReqID        string           `json:"req_id,omitempty"`
	Method       string           `json:"method,omitempty"`
	Params       json.RawMessage  `json:"params,omitempty"`
	Result       json.RawMessage  `json:"result,omitempty"`
	Error        string           `json:"error,omitempty"`
	Key          string           `json:"key,omitempty"`
	After        int64            `json:"after,omitempty"`
	Event        *cli.EventEntry  `json:"event,omitempty"`
	Events       []cli.EventEntry `json:"events,omitempty"`
	State        string           `json:"state,omitempty"`
	Reason       string           `json:"reason,omitempty"`
}
