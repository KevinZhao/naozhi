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
	// Model is populated for system/init events on stream-json (claude),
	// where the CLI advertises the resolved model identifier (e.g.
	// "global.anthropic.claude-opus-4-7[1m]"). readLoop forwards it to
	// Process.setModel so the dashboard can show the LIVE model rather
	// than the spawn-time SpawnOptions.Model — claude resolves env /
	// CLI defaults internally, so we never know the answer before init.
	// ACP path leaves this empty; SpawnOptions.Model from kiro config
	// is authoritative there. UI Round 5 R5-3.
	Model string `json:"model,omitempty"`

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
	// String-typed because kiro 2.3.0 emits UUID ids on session/request_permission;
	// numeric ids (rare on ACP requests-from-agent) are stringified by IDAsString
	// so HandleEvent always sees a stable opaque key. See rpc_types.go.
	RPCRequestID string `json:"-"`

	// RawParams carries the original JSON-RPC params for events that need
	// downstream inspection (e.g. permission_request whose options[] are
	// vendor-defined). Populated by ACPProtocol.ReadEvent; nil for events
	// that don't need it. Not serialized.
	RawParams json.RawMessage `json:"-"`

	// Metadata is populated for backend-emitted normalized status frames
	// (Type:"metadata"). Today the only producer is ACPProtocol's
	// _kiro.dev/metadata notification handler — kiro reports real
	// contextUsagePercentage / turnDurationMs / meteringUsage every turn.
	// Other backends (claude stream-json) leave this nil; the session layer
	// fills equivalent fields from CostUSD / wall clock. Dashboard reads
	// only the normalized SessionSnapshot fields, never this raw struct.
	// See docs/rfc/multi-backend.md §8.8.
	Metadata *EventMetadata `json:"metadata,omitempty"`

	// AskQuestion is populated for synthetic Type:"ask_question" events derived
	// from an AskUserQuestion tool_use in the assistant stream. The CLI's
	// headless -p mode auto-rejects the tool with is_error:true, so this is
	// observational only — dispatch uses it to surface an interactive card.
	// The user's answer flows back as a normal user message on the next turn.
	// See docs/rfc/askuser-question.md and test/e2e/askuser/.
	AskQuestion *AskQuestion `json:"ask_question,omitempty"`

	// ToolCall is populated for ACP tool_call / tool_call_update events.
	// Dashboard renders a progress row (pending → in_progress → completed
	// / failed) with collapsible rawOutput. Multi-Backend RFC §8.3 D17.
	// stream-json (Claude) leaves this nil — Claude tool-use events flow
	// through Message.Content[].Type=="tool_use" instead.
	ToolCall *ToolCall `json:"tool_call,omitempty"`

	// recvAt is the wall-clock moment readLoop pushed the event to eventCh.
	// Used by drainStaleEvents to distinguish events belonging to a previous
	// (possibly interrupted) turn from events produced for the current turn
	// after drain entered. Not serialized.
	recvAt time.Time
}

// EventMetadata mirrors the cross-backend "what just happened on this turn"
// status payload. Fields are optional — backends populate whichever
// equivalents they have. See ACPProtocol.parseKiroMetadata for kiro mapping
// and Process result-event handling for stream-json.
type EventMetadata struct {
	// ContextUsagePercent is the conversation context utilisation (0-100).
	// kiro: from _kiro.dev/metadata.contextUsagePercentage. claude: estimated
	// later by the session layer; cli leaves this 0 here.
	ContextUsagePercent float64 `json:"context_usage_percent,omitempty"`

	// TurnDurationMs is the duration of the just-completed turn, in ms.
	// kiro: from _kiro.dev/metadata.turnDurationMs. claude: filled by Process
	// from wall clock when a result event arrives; cli leaves this 0 here.
	TurnDurationMs int64 `json:"turn_duration_ms,omitempty"`

	// MeteringUsage is per-backend billing detail. kiro emits one or more
	// entries with {value, unit:"credit"}. claude leaves this empty (cost
	// already captured via CostUSD).
	MeteringUsage []MeteringEntry `json:"metering_usage,omitempty"`
}

// MeteringEntry is one row of a backend-reported billing payload, modelled
// after kiro 2.3.0's _kiro.dev/metadata.meteringUsage shape. Other backends
// can reuse the same struct (Unit "USD" works just as well as "credit").
type MeteringEntry struct {
	Value      float64 `json:"value"`
	Unit       string  `json:"unit"`
	UnitPlural string  `json:"unit_plural,omitempty"`
}

// ToolCall is the per-event payload for ACP tool_call / tool_call_update
// session/update notifications. Multi-Backend RFC §8.3 D17.
//
// Same struct serves both the initial "tool invocation" event (Status==""
// or "pending") and subsequent updates ("in_progress" / "completed" /
// "failed"). The dashboard threads them by ID — successive events with the
// same ID overwrite the prior progress row rather than appending.
//
// Output is the raw JSON payload kiro emits (kiro shape:
// `{"items":[{"Json":{"exit_status":"...", "stdout":"..."}}]}`); the
// dashboard decides how to extract a stdout string vs render JSON. Keeping
// it here as a string preserves the original formatting for "view raw".
type ToolCall struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Title      string `json:"title,omitempty"`
	Kind       string `json:"kind,omitempty"`        // "execute" / "read" / "write" / "search" / vendor-specific
	Status     string `json:"status,omitempty"`      // "" (initial) / "in_progress" / "completed" / "failed"
	InputJSON  string `json:"input_json,omitempty"`  // raw JSON of rawInput
	OutputJSON string `json:"output_json,omitempty"` // raw JSON of rawOutput
}

// AskQuestion mirrors the shape of AskUserQuestion.input observed against
// claude CLI 2.1.132 (see test/e2e/askuser/aq1_aq2_trigger_and_schema.py).
// ToolUseID is the tool_use id emitted by the assistant and serves as a
// correlation key across dashboard + IM renderings of the same question.
type AskQuestion struct {
	ToolUseID string            `json:"tool_use_id"`
	Items     []AskQuestionItem `json:"items"`
}

// AskQuestionItem is one question in a possibly multi-question card.
// MultiSelect=true signals checkbox semantics; the CLI may set it but the
// dashboard currently degrades to single-select (one click = one answer).
type AskQuestionItem struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	MultiSelect bool             `json:"multi_select,omitempty"`
	Options     []AskQuestionOpt `json:"options"`
}

// AskQuestionOpt is one selectable choice. Label is the user-facing text that
// the answer composer will echo back ("Header: Label."). Description is shown
// in the card tooltip / secondary line but never echoed.
type AskQuestionOpt struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// TaskUsage holds resource consumption stats from agent task events.
type TaskUsage struct {
	TotalTokens int   `json:"total_tokens"`
	ToolUses    int   `json:"tool_uses"`
	DurationMS  int64 `json:"duration_ms"`
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

// maxAssistantMessageContentBytes caps the total bytes of an
// AssistantMessage's content blocks accepted by ReadEvent. R229-SEC-10:
// a tampered or buggy CLI / shim could emit a single 10 MiB+ event whose
// nested content blocks are deeply structured but harmless-looking JSON;
// every downstream stage (EventLog ring, dashboard fan-out, JSONL persist)
// then pays O(N) per consumer. The 4 MiB ceiling sits comfortably above the
// largest legitimate content payload observed in production
// (base64 image + thinking block ≈ 1.5 MiB) yet caps single-event CPU /
// memory amplification well below the 10 MiB shim line cap.
const maxAssistantMessageContentBytes = 4 * 1024 * 1024

// contentBytes sums the user-visible byte size of an AssistantMessage's
// content blocks. Only fields that grow with model output are counted; the
// fixed-size discriminators (Type/ID/Name) are excluded so a message of
// many tiny tool_use blocks does not falsely trip the cap.
func contentBytes(m *AssistantMessage) int {
	if m == nil {
		return 0
	}
	total := 0
	for i := range m.Content {
		total += len(m.Content[i].Text)
		total += len(m.Content[i].Input)
	}
	return total
}

// UnmarshalJSON lets AssistantMessage tolerate a "content" field that is
// either the normal []ContentBlock (assistant messages, tool_result users)
// or a plain string (CLI's replay-user-messages echoes the original user
// payload, and when the user sent a text-only message the CLI emits
// "content": "...text..." instead of a block array).
//
// We can't silently fall back to a single text-block array for *all* string
// shapes, because tool_result user events also encode content as an array.
// Only the shape normalization happens here; downstream consumers handle the
// single-text case uniformly via the resulting ContentBlock array.
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
	// Route on the first non-whitespace byte to avoid speculatively
	// Unmarshal-ing into []ContentBlock when the shape is a bare string.
	// The old two-try fallback allocated (and partially filled) a
	// []ContentBlock slice every time we hit the replay-text path before
	// discarding it.
	first := firstJSONByte(raw.Content)
	switch first {
	case '[':
		var blocks []ContentBlock
		if err := json.Unmarshal(raw.Content, &blocks); err == nil {
			m.Content = blocks
			return nil
		}
	case '"':
		var text string
		if err := json.Unmarshal(raw.Content, &text); err == nil {
			m.Content = []ContentBlock{{Type: "text", Text: text}}
			return nil
		}
	}
	// Unknown shape: leave Content nil so downstream code treats it as empty
	// rather than erroring the whole event.
	m.Content = nil
	return nil
}

// firstJSONByte returns the first non-whitespace byte of a JSON raw message,
// or 0 if the buffer is empty or whitespace-only.
func firstJSONByte(raw []byte) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b
		}
	}
	return 0
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
//
// Deprecated: Use Attachment directly with an explicit Kind field.
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
