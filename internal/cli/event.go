package cli

import "encoding/json"

// Event represents a parsed stream-json event from claude CLI stdout.
type Event struct {
	Type      string            `json:"type"`
	SubType   string            `json:"subtype,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Result    string            `json:"result,omitempty"`
	CostUSD   float64           `json:"total_cost_usd,omitempty"`
	Message   *AssistantMessage `json:"message,omitempty"`

	// RPCRequestID is set for ACP permission_request events that need a response.
	RPCRequestID int `json:"-"`
}

type AssistantMessage struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`  // tool_use name
	Input json.RawMessage `json:"input,omitempty"` // tool_use input
}

// InputMessage is what we write to claude CLI stdin.
type InputMessage struct {
	Type    string       `json:"type"`
	Message InputContent `json:"message"`
}

type InputContent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// NewUserMessage creates the NDJSON input for a user message.
func NewUserMessage(text string) InputMessage {
	return InputMessage{
		Type: "user",
		Message: InputContent{
			Role:    "user",
			Content: text,
		},
	}
}

// SendResult is returned by Process.Send.
type SendResult struct {
	Text      string
	SessionID string
	CostUSD   float64
}
