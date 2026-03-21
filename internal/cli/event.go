package cli

import (
	"encoding/base64"
	"encoding/json"
)

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

// ImageData holds a downloaded image for passing to the CLI.
type ImageData struct {
	Data     []byte
	MimeType string
}

// InputMessage is what we write to claude CLI stdin.
type InputMessage struct {
	Type    string       `json:"type"`
	Message InputContent `json:"message"`
}

type InputContent struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string (text-only) or []any (multimodal)
}

// inputTextBlock is a text content block for multimodal messages.
type inputTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// inputImageBlock is an image content block for multimodal messages.
type inputImageBlock struct {
	Type   string      `json:"type"`
	Source imageSource `json:"source"`
}

type imageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // e.g., "image/png"
	Data      string `json:"data"`       // base64-encoded
}

// NewUserMessage creates the NDJSON input for a user message.
// When images is non-empty, content is formatted as multimodal content blocks.
func NewUserMessage(text string, images []ImageData) InputMessage {
	var content any
	if len(images) == 0 {
		content = text
	} else {
		blocks := make([]any, 0, 1+len(images))
		for _, img := range images {
			blocks = append(blocks, inputImageBlock{
				Type: "image",
				Source: imageSource{
					Type:      "base64",
					MediaType: img.MimeType,
					Data:      base64.StdEncoding.EncodeToString(img.Data),
				},
			})
		}
		if text != "" {
			blocks = append(blocks, inputTextBlock{Type: "text", Text: text})
		}
		content = blocks
	}
	return InputMessage{
		Type: "user",
		Message: InputContent{
			Role:    "user",
			Content: content,
		},
	}
}

// SendResult is returned by Process.Send.
type SendResult struct {
	Text      string
	SessionID string
	CostUSD   float64
}
