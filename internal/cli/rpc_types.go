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
// ID is json.RawMessage rather than *int because JSON-RPC 2.0 allows a string,
// number or null id and kiro's session/request_permission emits string UUIDs;
// a *int decoder would fail and kill the readLoop. IDAsInt / IDAsString cover
// both shapes.
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
	// Kind classifies the tool category ("execute", "read", "write",
	// "search"); the dashboard picks the progress-row icon from it.
	Kind string `json:"kind,omitempty"`
	// RawInput / RawOutput stay RawMessage: only the dashboard decodes them
	// (JSON view or kiro-specific stdout extraction).
	RawInput  json.RawMessage `json:"rawInput,omitempty"`
	RawOutput json.RawMessage `json:"rawOutput,omitempty"`
}

// ACPTextContent represents a text content block in ACP events.
type ACPTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ACPSessionNewResult is the result of session/new (and structurally of
// session/load — kiro returns the same models envelope on both).
type ACPSessionNewResult struct {
	SessionID string `json:"sessionId"`
	// Models is the agent's model manifest (current selection + availableModels)
	// feeding the dashboard model popover; nil on agents that don't report it.
	Models *ACPModelsEnvelope `json:"models,omitempty"`
}

// ACPModelsEnvelope matches the "models" object in session/new|load results.
type ACPModelsEnvelope struct {
	CurrentModelID  string         `json:"currentModelId"`
	AvailableModels []ACPModelInfo `json:"availableModels"`
}

// ACPModelInfo is one selectable model as reported by the agent.
type ACPModelInfo struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ModelInfo is the protocol-agnostic model-manifest entry naozhi caches and
// serves via /api/cli/backends. JSON tags are the dashboard wire shape.
type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// ACPPermissionRequestParams is the params of a session/request_permission
// request. HandleEvent picks the optionId whose Kind matches the desired
// allow_* rather than hardcoding it: kiro uses underscored names while the
// ACP draft documents hyphenated ones, so the value must come from the request.
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
