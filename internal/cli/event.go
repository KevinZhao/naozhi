package cli

import (
	"encoding/base64"
	"encoding/json"
	"time"
)

// Event represents a parsed stream-json event from claude CLI stdout.
type Event struct {
	Type      string            `json:"type"`
	SubType   string            `json:"subtype,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Result    string            `json:"result,omitempty"`
	CostUSD   float64           `json:"total_cost_usd,omitempty"`
	Message   *AssistantMessage `json:"message,omitempty"`

	// Agent task fields (system/task_started, task_progress, task_notification).
	TaskID       string     `json:"task_id,omitempty"`
	ToolUseID    string     `json:"tool_use_id,omitempty"`
	Description  string     `json:"description,omitempty"`
	TaskType     string     `json:"task_type,omitempty"`
	Status       string     `json:"status,omitempty"`
	LastToolName string     `json:"last_tool_name,omitempty"`
	Usage        *TaskUsage `json:"usage,omitempty"`

	// Passthrough fields (stream-json only). UUID is the Claude CLI uuid
	// round-tripped on replay events (see --replay-user-messages). IsReplay
	// distinguishes ack echoes from genuine user events (tool_result / system
	// messages). Both are ignored outside the passthrough slot-matching path.
	UUID     string `json:"uuid,omitempty"`
	IsReplay bool   `json:"isReplay,omitempty"`

	// RPCRequestID is set for ACP permission_request events that need a response.
	RPCRequestID int `json:"-"`

	// recvAt is the wall-clock moment readLoop pushed the event to eventCh.
	// Used by drainStaleEvents to distinguish events belonging to a previous
	// (possibly interrupted) turn from events produced for the current turn
	// after drain entered. Not serialized.
	recvAt time.Time
}

// TaskUsage holds resource consumption stats from agent task events.
type TaskUsage struct {
	TotalTokens int `json:"total_tokens"`
	ToolUses    int `json:"tool_uses"`
	DurationMS  int `json:"duration_ms"`
}

type AssistantMessage struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

type ContentBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"` // tool_use id (for agent→task linking)
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`  // tool_use name
	Input json.RawMessage `json:"input,omitempty"` // tool_use input
}

// UnmarshalJSON lets AssistantMessage tolerate a "content" field that is
// either the normal []ContentBlock (assistant messages, tool_result users)
// or a plain string (CLI's replay-user-messages echoes the original user
// payload, and when the user sent a text-only message the CLI emits
// "content": "...text..." instead of a block array).
//
// We can't silently fall back to a single text-block array for *all* string
// shapes, because tool_result user events also encode content as an array.
// Only the shape normalization happens here; downstream extractReplayTexts
// handles the single-text case uniformly.
func (m *AssistantMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	if len(raw.Content) == 0 {
		m.Content = nil
		return nil
	}
	// Try array first (most events).
	var blocks []ContentBlock
	if err := json.Unmarshal(raw.Content, &blocks); err == nil {
		m.Content = blocks
		return nil
	}
	// Fall back to string (replay-user-messages text-only payload).
	var text string
	if err := json.Unmarshal(raw.Content, &text); err == nil {
		m.Content = []ContentBlock{{Type: "text", Text: text}}
		return nil
	}
	// Unknown shape: leave Content nil so downstream code treats it as empty
	// rather than erroring the whole event.
	m.Content = nil
	return nil
}

// ImageData holds a downloaded image for passing to the CLI.
type ImageData struct {
	Data     []byte
	MimeType string
}

// InputMessage is what we write to claude CLI stdin.
//
// UUID: naozhi-assigned message id, round-tripped back on the matching replay
// event when --replay-user-messages is enabled. Used for passthrough slot
// matching (see docs/rfc/passthrough-mode.md §5.2). Omitted when empty to
// stay compatible with legacy non-passthrough writers.
//
// Priority: one of "now" | "next" | "later" | "". An empty string lets the
// CLI default (currently "next") kick in. "now" causes the CLI to abort the
// in-flight turn (verified via V2 — print.ts:1858-1863). Ignored by protocols
// that do not advertise SupportsPriority().
type InputMessage struct {
	Type     string       `json:"type"`
	Message  InputContent `json:"message"`
	UUID     string       `json:"uuid,omitempty"`
	Priority string       `json:"priority,omitempty"`
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
	return NewUserMessageWithMeta(text, images, "", "")
}

// NewUserMessageWithMeta is the passthrough-aware constructor. When uuid /
// priority are empty strings they are omitted from the JSON (legacy-identical
// payload). When non-empty they are serialised as top-level fields.
//
// The CLI (verified against 2.1.126) accepts any top-level uuid/priority on
// the NDJSON user message and round-trips uuid on the corresponding replay
// event. Priority "now" is an explicit abort signal (print.ts:1858-1863).
func NewUserMessageWithMeta(text string, images []ImageData, uuid, priority string) InputMessage {
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
		UUID:     uuid,
		Priority: priority,
	}
}

// SendResult is returned by Process.Send. In passthrough mode multiple slots
// can share one upstream turn result — MergedCount>1 signals such a merge and
// MergedWithHead identifies the head slot whose caller got the full text.
// Follower slots receive MergedCount>1 with Text=="" so the dispatch layer
// can surface a "merged into previous reply" reaction instead of re-sending
// the same text multiple times (see docs/rfc/passthrough-mode.md §6.1.3).
type SendResult struct {
	Text      string
	SessionID string
	CostUSD   float64

	// Merge metadata. Zero means "single-slot result, no merge".
	MergedCount    int    // total slots sharing this result (>=2 in a merge)
	MergedWithHead uint64 // 0 for head; for follower the id of the head sendSlot
	HeadText       string // follower mirror of Text (optional, for UI association)
}
