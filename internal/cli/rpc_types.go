package cli

import "encoding/json"

// RPCRequest is a JSON-RPC 2.0 request.
type RPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// RPCMessage is a generic JSON-RPC 2.0 message (request, response, or notification).
type RPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
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

// IsNotification returns true if this message has no ID (a JSON-RPC notification).
func (m *RPCMessage) IsNotification() bool {
	return m.ID == nil && m.Method != ""
}

// IsResponse returns true if this is a response (has ID, no method).
func (m *RPCMessage) IsResponse() bool {
	return m.ID != nil && m.Method == ""
}

// IsRequest returns true if this is a request (has ID and method).
func (m *RPCMessage) IsRequest() bool {
	return m.ID != nil && m.Method != ""
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

// ACPPermissionRequest represents session/request_permission params.
type ACPPermissionRequest struct {
	SessionID string `json:"sessionId"`
	Options   []struct {
		OptionID string `json:"optionId"`
		Kind     string `json:"kind"`
	} `json:"options"`
}
