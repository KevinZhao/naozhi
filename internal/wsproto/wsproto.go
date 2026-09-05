// Package wsproto is the single source of truth for the browser WebSocket
// protocol (#2535): every message type the dashboard can receive or send is
// a constant here, and every outbound frame is a per-type struct whose JSON
// shape is pinned byte-for-byte against the legacy node.ServerMsg flat union
// (golden tests in frames_golden_test.go). The node RPC protocol
// (node.ReverseMsg, ProtocolVersion handshake) is deliberately NOT here —
// wsproto is the browser wire only.
//
// Frames are constructed through the New* functions, which stamp Type;
// construction sites never write a type literal. Field declaration order
// inside every frame follows node.ServerMsg's order so the marshaled bytes
// stay identical through the migration (relay passes frames through as raw
// bytes; byte identity is the cheap way to prove ~100 mechanical call-site
// rewrites changed nothing).
//
//go:generate go run ./gen
package wsproto

import "github.com/naozhi/naozhi/internal/cli/clievent"

// MsgType names one browser WS message type.
type MsgType string

// Outbound (server → browser) message types.
const (
	TypeAuthOK                 MsgType = "auth_ok"
	TypeAuthFail               MsgType = "auth_fail"
	TypePong                   MsgType = "pong"
	TypeError                  MsgType = "error"
	TypeSubscribed             MsgType = "subscribed"
	TypeUnsubscribed           MsgType = "unsubscribed"
	TypeHistory                MsgType = "history"
	TypeEvent                  MsgType = "event"
	TypeSendAck                MsgType = "send_ack"
	TypeSendError              MsgType = "send_error"
	TypeInterruptAck           MsgType = "interrupt_ack"
	TypeSessionState           MsgType = "session_state"
	TypeSessionsUpdate         MsgType = "sessions_update"
	TypeCronRunStarted         MsgType = "cron_run_started"
	TypeCronRunEnded           MsgType = "cron_run_ended"
	TypeDaemonRunStarted       MsgType = "daemon_run_started"
	TypeDaemonRunEnded         MsgType = "daemon_run_ended"
	TypeAgentEvent             MsgType = "agent_event"
	TypeAgentMeta              MsgType = "agent_meta"
	TypeAgentDone              MsgType = "agent_done"
	TypeAgentSubscribeRejected MsgType = "agent_subscribe_rejected"
)

// Inbound (browser → server) message types, parsed out of node.ClientMsg.
const (
	TypeAuth             MsgType = "auth"
	TypeSubscribe        MsgType = "subscribe"
	TypeUnsubscribe      MsgType = "unsubscribe"
	TypeSend             MsgType = "send"
	TypeInterrupt        MsgType = "interrupt"
	TypePing             MsgType = "ping"
	TypeAgentSubscribe   MsgType = "agent_subscribe"
	TypeAgentUnsubscribe MsgType = "agent_unsubscribe"
)

// AgentMetaPatch carries aggregator counters pushed out-of-band so the
// dashboard can refresh a banner row without re-rendering the agent view.
// node.AgentMetaPatch aliases this type.
type AgentMetaPatch struct {
	LastTool   string `json:"last_tool,omitempty"`
	LastDetail string `json:"last_detail,omitempty"`
	ToolUses   int    `json:"tool_uses,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// ── outbound frames ─────────────────────────────────────────────────────────
//
// One struct per type, fields limited to what that type actually carries.
// Callers go through New*; writing a frame literal without it leaves Type
// empty and the frame registry test red.

type AuthOK struct {
	Type MsgType `json:"type"`
}

func NewAuthOK() AuthOK { return AuthOK{Type: TypeAuthOK} }

type AuthFail struct {
	Type  MsgType `json:"type"`
	Error string  `json:"error,omitempty"`
	// RetryAfter (seconds, advisory) is set only on rate-limit auth_fail,
	// mirroring the HTTP Retry-After header on /api/auth/login 429.
	RetryAfter int `json:"retry_after,omitempty"`
}

func NewAuthFail(f AuthFail) AuthFail { f.Type = TypeAuthFail; return f }

type Pong struct {
	Type MsgType `json:"type"`
}

func NewPong() Pong { return Pong{Type: TypePong} }

type Error struct {
	Type  MsgType `json:"type"`
	Key   string  `json:"key,omitempty"`
	Error string  `json:"error,omitempty"`
	Node  string  `json:"node,omitempty"`
}

func NewError(f Error) Error { f.Type = TypeError; return f }

type Subscribed struct {
	Type   MsgType `json:"type"`
	Key    string  `json:"key,omitempty"`
	State  string  `json:"state,omitempty"`
	Reason string  `json:"reason,omitempty"`
	Node   string  `json:"node,omitempty"`
}

func NewSubscribed(f Subscribed) Subscribed { f.Type = TypeSubscribed; return f }

type Unsubscribed struct {
	Type MsgType `json:"type"`
	Key  string  `json:"key,omitempty"`
	Node string  `json:"node,omitempty"`
}

func NewUnsubscribed(f Unsubscribed) Unsubscribed { f.Type = TypeUnsubscribed; return f }

type History struct {
	Type   MsgType               `json:"type"`
	Key    string                `json:"key,omitempty"`
	Events []clievent.EventEntry `json:"events,omitempty"`
	Node   string                `json:"node,omitempty"`
	// HasMore, on the initial frame, says whether events older than the
	// slice exist. *bool so an explicit false still serializes; nil means
	// "fall back to the client's length heuristic".
	HasMore *bool `json:"has_more,omitempty"`
	// Initial marks the opening page the dashboard renders by replacing the
	// pane; every incremental append MUST leave it false.
	Initial bool `json:"initial,omitempty"`
}

func NewHistory(f History) History { f.Type = TypeHistory; return f }

type Event struct {
	Type  MsgType              `json:"type"`
	Key   string               `json:"key,omitempty"`
	Event *clievent.EventEntry `json:"event,omitempty"`
	Node  string               `json:"node,omitempty"`
}

func NewEvent(f Event) Event { f.Type = TypeEvent; return f }

type SendAck struct {
	Type   MsgType `json:"type"`
	Key    string  `json:"key,omitempty"`
	ID     string  `json:"id,omitempty"`
	Status string  `json:"status,omitempty"`
	Error  string  `json:"error,omitempty"`
	Node   string  `json:"node,omitempty"`
}

func NewSendAck(f SendAck) SendAck { f.Type = TypeSendAck; return f }

type SendError struct {
	Type  MsgType `json:"type"`
	Key   string  `json:"key,omitempty"`
	Error string  `json:"error,omitempty"`
	Node  string  `json:"node,omitempty"`
}

func NewSendError(f SendError) SendError { f.Type = TypeSendError; return f }

type InterruptAck struct {
	Type   MsgType `json:"type"`
	Key    string  `json:"key,omitempty"`
	ID     string  `json:"id,omitempty"`
	Status string  `json:"status,omitempty"`
	Error  string  `json:"error,omitempty"`
	Node   string  `json:"node,omitempty"`
}

func NewInterruptAck(f InterruptAck) InterruptAck { f.Type = TypeInterruptAck; return f }

type SessionState struct {
	Type   MsgType `json:"type"`
	Key    string  `json:"key,omitempty"`
	State  string  `json:"state,omitempty"`
	Reason string  `json:"reason,omitempty"`
	Node   string  `json:"node,omitempty"`
}

func NewSessionState(f SessionState) SessionState { f.Type = TypeSessionState; return f }

type SessionsUpdate struct {
	Type MsgType `json:"type"`
}

func NewSessionsUpdate() SessionsUpdate { return SessionsUpdate{Type: TypeSessionsUpdate} }

// CronRunStarted / CronRunEnded / DaemonRunStarted / DaemonRunEnded moved
// verbatim from internal/server/wshub_broadcast.go's private structs.

type CronRunStarted struct {
	Type      MsgType `json:"type"`
	JobID     string  `json:"job_id"`
	RunID     string  `json:"run_id"`
	StartedAt int64   `json:"started_at"`
	Trigger   string  `json:"trigger,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	Fresh     bool    `json:"fresh,omitempty"`
}

func NewCronRunStarted(f CronRunStarted) CronRunStarted { f.Type = TypeCronRunStarted; return f }

type CronRunEnded struct {
	Type       MsgType `json:"type"`
	JobID      string  `json:"job_id"`
	RunID      string  `json:"run_id"`
	State      string  `json:"state"`
	StartedAt  int64   `json:"started_at"`
	EndedAt    int64   `json:"ended_at"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	SessionID  string  `json:"session_id,omitempty"`
	ErrorClass string  `json:"error_class,omitempty"`
	ErrorMsg   string  `json:"error_msg,omitempty"`
	Trigger    string  `json:"trigger,omitempty"`
}

func NewCronRunEnded(f CronRunEnded) CronRunEnded { f.Type = TypeCronRunEnded; return f }

type DaemonRunStarted struct {
	Type      MsgType `json:"type"`
	Name      string  `json:"name"`
	RunID     string  `json:"run_id"`
	Trigger   string  `json:"trigger,omitempty"`
	StartedAt int64   `json:"started_at"`
}

func NewDaemonRunStarted(f DaemonRunStarted) DaemonRunStarted {
	f.Type = TypeDaemonRunStarted
	return f
}

// DaemonRunEnded deliberately has no ErrorMsg: daemon error text can carry
// cross-tenant detail; the class is enough for the dashboard's badge.
type DaemonRunEnded struct {
	Type       MsgType `json:"type"`
	Name       string  `json:"name"`
	RunID      string  `json:"run_id"`
	State      string  `json:"state"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	ErrorClass string  `json:"error_class,omitempty"`
	Trigger    string  `json:"trigger,omitempty"`
}

func NewDaemonRunEnded(f DaemonRunEnded) DaemonRunEnded { f.Type = TypeDaemonRunEnded; return f }

type AgentEvent struct {
	Type   MsgType              `json:"type"`
	Key    string               `json:"key,omitempty"`
	Event  *clievent.EventEntry `json:"event,omitempty"`
	TaskID string               `json:"task_id,omitempty"`
}

func NewAgentEvent(f AgentEvent) AgentEvent { f.Type = TypeAgentEvent; return f }

type AgentMeta struct {
	Type      MsgType         `json:"type"`
	Key       string          `json:"key,omitempty"`
	TaskID    string          `json:"task_id,omitempty"`
	AgentMeta *AgentMetaPatch `json:"meta,omitempty"`
}

func NewAgentMeta(f AgentMeta) AgentMeta { f.Type = TypeAgentMeta; return f }

type AgentDone struct {
	Type   MsgType `json:"type"`
	Key    string  `json:"key,omitempty"`
	Status string  `json:"status,omitempty"`
	TaskID string  `json:"task_id,omitempty"`
}

func NewAgentDone(f AgentDone) AgentDone { f.Type = TypeAgentDone; return f }

type AgentSubscribeRejected struct {
	Type   MsgType `json:"type"`
	Key    string  `json:"key,omitempty"`
	Reason string  `json:"reason,omitempty"`
	TaskID string  `json:"task_id,omitempty"`
}

func NewAgentSubscribeRejected(f AgentSubscribeRejected) AgentSubscribeRejected {
	f.Type = TypeAgentSubscribeRejected
	return f
}
