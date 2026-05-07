package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
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

// AttachmentKind discriminates between inline and file-reference attachments.
//
//   - KindImageInline: raw bytes forwarded as an Anthropic image content block.
//   - KindFileRef:     a file already written to the session workspace; CLI is
//     asked to read it via its native Read tool. Used for PDFs whose base64
//     encoding would exceed the 12 MB stdin line cap — see
//     docs/rfc/pdf-attachment.md §2.1.
const (
	KindImageInline = "image_inline"
	KindFileRef     = "file_ref"
)

// Attachment is an inline user-message asset (image bytes) or a workspace
// file reference (PDF). The dispatch → coalesce → protocol chain passes
// []Attachment end-to-end; NewUserMessageWithMeta decides per-element
// whether to emit an image content block, omit the element (file_ref is
// handled through the text prefix), or surface the reference in a
// prepended instruction to Claude.
type Attachment struct {
	// Kind is KindImageInline (default, zero value means image for legacy
	// call sites) or KindFileRef.
	Kind string

	// Data holds raw bytes when Kind==KindImageInline. Nil for file_ref.
	Data []byte

	// MimeType is always set. For file_ref, this is the content type of the
	// workspace file (e.g. "application/pdf").
	MimeType string

	// WorkspacePath is a project-root-relative path to a file written by
	// naozhi into the session workspace. Only meaningful for KindFileRef.
	// Always uses forward slashes even on Windows so it can be pasted into
	// the CLI Read tool directly.
	WorkspacePath string

	// OrigName is the user-provided filename at upload time, preserved for
	// UI display and the prepended CLI instruction. May be empty.
	OrigName string

	// Size is the byte size of the on-disk file for KindFileRef; 0 for
	// image_inline (Data carries the bytes). Used only for display.
	Size int64
}

// ImageData is retained as a type alias for the pre-PDF call sites. New code
// uses Attachment directly with an explicit Kind. The alias keeps legacy
// constructors (many tests, dispatch/coalesce, platform adapters) compiling
// without edits; migrations that promote Kind/WorkspacePath fields happen on
// a case-by-case basis. Final removal is tracked in docs/TODO.md.
type ImageData = Attachment

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

// splitAttachments partitions atts into inline (image) and file-reference
// slices while preserving original order within each bucket. The zero-value
// Kind ("") is treated as KindImageInline so legacy call sites that never
// set Kind continue to behave exactly as before.
func splitAttachments(atts []Attachment) (inline []Attachment, refs []Attachment) {
	for _, a := range atts {
		switch a.Kind {
		case KindFileRef:
			refs = append(refs, a)
		default: // "" and KindImageInline
			inline = append(inline, a)
		}
	}
	return inline, refs
}

// prependFileRefHint returns text with a Read-tool instruction prepended when
// refs is non-empty. When refs is empty the original text is returned
// unchanged (byte-identical to the pre-PDF code path — important because it
// keeps the stream-json NDJSON wire form stable for image-only sends).
//
// The hint intentionally mentions workspace-relative paths with forward
// slashes: the CLI Read tool resolves relative paths against the session
// working directory (SpawnOptions.WorkingDir) which naozhi always sets to
// the resolved workspace root (see internal/session/router.go:1629).
func prependFileRefHint(text string, refs []Attachment) string {
	if len(refs) == 0 {
		return text
	}
	var b strings.Builder
	// Rough pre-allocation: ~80 bytes header + ~120 bytes per ref + user text.
	b.Grow(80 + 120*len(refs) + len(text))
	if len(refs) == 1 {
		b.WriteString("[System: The user attached 1 file to the workspace. ")
	} else {
		fmt.Fprintf(&b, "[System: The user attached %d files to the workspace. ", len(refs))
	}
	b.WriteString("Read the following file(s) with the Read tool before responding:\n")
	for _, r := range refs {
		p := r.WorkspacePath
		// The Read tool accepts both absolute and workspace-relative paths;
		// we always write forward-slash relative paths so the same string
		// shown to the user in the dashboard is what Claude sees.
		if p == "" {
			// A file_ref without a WorkspacePath is a caller bug — skip it
			// rather than injecting an empty bullet that would confuse the
			// model. The sender pipeline is expected to populate this before
			// calling NewUserMessageWithMeta.
			continue
		}
		b.WriteString("  - ")
		b.WriteString(p)
		if r.OrigName != "" && r.OrigName != p {
			b.WriteString(" (original name: ")
			b.WriteString(r.OrigName)
			b.WriteString(")")
		}
		if r.Size > 0 {
			fmt.Fprintf(&b, " [%s]", formatBytesShort(r.Size))
		}
		b.WriteString("\n")
	}
	b.WriteString("]\n\n")
	if text != "" {
		b.WriteString(text)
	}
	return b.String()
}

// formatBytesShort renders a byte count as a human-friendly short string
// (e.g. 1.2 MB). Used only inside the attachment hint; precision is
// intentionally coarse so the prompt size is predictable.
func formatBytesShort(n int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
	)
	switch {
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%d KB", n/kb)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// NewUserMessageWithMeta is the passthrough-aware constructor. When uuid /
// priority are empty strings they are omitted from the JSON (legacy-identical
// payload). When non-empty they are serialised as top-level fields.
//
// The CLI (verified against 2.1.126) accepts any top-level uuid/priority on
// the NDJSON user message and round-trips uuid on the corresponding replay
// event. Priority "now" is an explicit abort signal (print.ts:1858-1863).
func NewUserMessageWithMeta(text string, atts []Attachment, uuid, priority string) InputMessage {
	// file_ref attachments do NOT produce a content block — they are surfaced
	// to Claude via a prepended instruction in the text, so the CLI's native
	// Read tool picks them up. Split once here so subsequent logic only has to
	// reason about inline bytes.
	inline, refs := splitAttachments(atts)

	// Prepend the Read-tool hint before the user's own text. The hint is in
	// English because the CC base system prompt is English-primary and
	// language-mixed prompts have been observed to cause the model to switch
	// reply language unpredictably. Original filenames (often Chinese) are
	// preserved verbatim in the hint for user recognition.
	effectiveText := prependFileRefHint(text, refs)

	var content any
	if len(inline) == 0 {
		content = effectiveText
	} else {
		blocks := make([]any, 0, 1+len(inline))
		for _, img := range inline {
			blocks = append(blocks, inputImageBlock{
				Type: "image",
				Source: imageSource{
					Type:      "base64",
					MediaType: img.MimeType,
					Data:      base64.StdEncoding.EncodeToString(img.Data),
				},
			})
		}
		if effectiveText != "" {
			blocks = append(blocks, inputTextBlock{Type: "text", Text: effectiveText})
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
