package cli

import (
	"encoding/json"
	"strconv"
)

// RPCRequest is a JSON-RPC 2.0 request.
type RPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RPCMessage is a generic JSON-RPC 2.0 message (request, response, or notification).
//
// ID is json.RawMessage rather than *int because the JSON-RPC 2.0 spec allows
// id to be a string, number, or null, and at least one ACP backend (kiro
// 2.3.0's session/request_permission, observed 2026-05-18) emits string UUIDs
// like "82017692-c404-42d1-9334-ae28dfda0cee". A *int decoder would fail with
// "cannot unmarshal string into Go struct field RPCMessage.id of type int" and
// kill the whole readLoop. Helpers IDAsInt / IDAsString cover both shapes.
type RPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError represents a JSON-RPC 2.0 error.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// hasID reports whether the message carries an id field. Only an explicit
// JSON null counts as "absent" together with the omitted case so a notification
// (no id) is distinguishable from a response (id can be 0 / "" but not null).
func (m *RPCMessage) hasID() bool {
	if len(m.ID) == 0 {
		return false
	}
	// "null" literal is treated as absent — the spec's only sentinel for a
	// response to an unparseable request, which we never originate here.
	if len(m.ID) == 4 && string(m.ID) == "null" {
		return false
	}
	return true
}

// IsNotification returns true if this message has no ID (a JSON-RPC notification).
func (m *RPCMessage) IsNotification() bool {
	return !m.hasID() && m.Method != ""
}

// IsResponse returns true if this is a response (has ID, no method).
func (m *RPCMessage) IsResponse() bool {
	return m.hasID() && m.Method == ""
}

// IsRequest returns true if this is a request (has ID and method).
func (m *RPCMessage) IsRequest() bool {
	return m.hasID() && m.Method != ""
}

// IDAsInt decodes the id as an integer. Returns ok=false when the id is a
// string (kiro UUID), null, or absent, even when the integer parse fails.
func (m *RPCMessage) IDAsInt() (int, bool) {
	if !m.hasID() {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(m.ID, &n); err == nil {
		return n, true
	}
	return 0, false
}

// IDAsString decodes the id as a string. Numeric ids are stringified
// (strconv.Itoa) so this accessor is the safe choice when the caller just
// needs a stable opaque key — for example to round-trip in a permission
// response. Returns ok=false only when the id is absent / null.
func (m *RPCMessage) IDAsString() (string, bool) {
	if !m.hasID() {
		return "", false
	}
	if n, ok := m.IDAsInt(); ok {
		return strconv.Itoa(n), true
	}
	var s string
	if err := json.Unmarshal(m.ID, &s); err == nil {
		return s, true
	}
	return "", false
}

// ACPSessionUpdate represents the params of a session/update notification.
type ACPSessionUpdate struct {
	SessionID string          `json:"sessionId"`
	Update    ACPUpdateDetail `json:"update"`
}

// ACPUpdateDetail holds the inner update payload.
type ACPUpdateDetail struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content,omitempty"`
	ToolCallID    string          `json:"toolCallId,omitempty"`
	Title         string          `json:"title,omitempty"`
	Status        string          `json:"status,omitempty"`
	// Kind classifies the tool category — e.g. "execute", "read", "write",
	// "search". Multi-Backend RFC §8.3 D17 / V7 sample. Used by dashboard
	// to pick a representative icon when rendering the progress row.
	Kind string `json:"kind,omitempty"`
	// RawInput / RawOutput are the tool's argument / result payloads. Kept
	// as RawMessage so the dashboard can decide whether to render JSON or
	// to extract a stdout string from the kiro-specific shape; cli/server
	// don't need to decode them. RFC §8.3 D17 collapsible output panel.
	RawInput  json.RawMessage `json:"rawInput,omitempty"`
	RawOutput json.RawMessage `json:"rawOutput,omitempty"`
}

// ACPTextContent represents a text content block in ACP events.
type ACPTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ACPSessionNewResult is the result of session/new.
type ACPSessionNewResult struct {
	SessionID string `json:"sessionId"`
}

// ACPPermissionRequestParams is the params of a session/request_permission
// request. naozhi's HandleEvent inspects Options to pick the optionId that
// matches the desired allow_* kind rather than hardcoding the string —
// kiro 2.3.0 uses underscored names (allow_once / allow_always / reject_once)
// while the original ACP draft documented hyphenated forms, so the value
// must come from the request.
type ACPPermissionRequestParams struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string `json:"toolCallId"`
		Title      string `json:"title"`
	} `json:"toolCall"`
	Options []ACPPermissionOption `json:"options"`
}

// ACPPermissionOption is one selectable choice in a permission request.
type ACPPermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // "allow_once" / "allow_always" / "reject_once"
}
