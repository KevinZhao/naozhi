package cron

import "encoding/json"

// transcriptResponse is the wire shape the dashboard consumes.
type transcriptResponse struct {
	SessionID string            `json:"session_id,omitempty"`
	StartedAt int64             `json:"started_at,omitempty"`
	EndedAt   int64             `json:"ended_at,omitempty"`
	Tokens    *transcriptTokens `json:"tokens,omitempty"`
	ToolCalls int               `json:"tool_calls"`
	Turns     []transcriptTurn  `json:"turns"`
	NextIndex int               `json:"next_index"`
	Truncated bool              `json:"truncated"`
	// TruncateReason discriminates why Truncated is true so forensics can tell
	// a size-cap hit from a disk read error or an over-long line (#1049).
	// Only populated when Truncated is true:
	//   "size_cap"       — hit maxTranscriptBytes / maxTranscriptTurns
	//   "line_too_long"  — bufio.ErrTooLong (line > maxTranscriptLineBytes)
	//   "scan_io_error"  — Scanner.Err returned a non-ErrTooLong error
	TruncateReason string `json:"truncate_reason,omitempty"`
	// Fallback signals a degraded path:
	//   "missing" — SessionID empty or JSONL not found
	//   "raw"     — JSONL exists but no turns parsed
	//   ""        — normal path
	Fallback string `json:"fallback,omitempty"`
}
type transcriptTokens struct {
	Input  int `json:"input"`
	Output int `json:"output"`
	Total  int `json:"total"`
}

// transcriptTurn is a single rendered timeline entry. Only fields relevant to
// its kind are populated (omitempty). Index is the position in the *response*,
// not the JSONL line — the dashboard uses it as a stable key for live diffs.
//
// CLIENT-SIDE CONTRACT (#921): Input is forwarded as raw JSON bytes from the
// CLI's tool_use payload and httputil.WriteJSON disables SetEscapeHTML, so
// `<`, `>`, `&` survive verbatim. Tool input is attacker-influenced (a
// malicious project file can steer the CLI's tool calls), so any consumer
// must render it via JSON.stringify + esc() or DOMPurify — never raw
// innerHTML. maxToolInputBytes bounds its size but does not normalise bytes.
type transcriptTurn struct {
	Index      int             `json:"index"`
	Kind       string          `json:"kind"` // "user" | "assistant" | "tool_use" | "tool_result" | "error"
	TS         int64           `json:"ts,omitempty"`
	Text       string          `json:"text,omitempty"`        // user / assistant / error
	Tokens     int             `json:"tokens,omitempty"`      // assistant only (output token delta)
	Tool       string          `json:"tool,omitempty"`        // tool_use
	ToolUseID  string          `json:"tool_use_id,omitempty"` // tool_use / tool_result link
	Summary    string          `json:"summary,omitempty"`     // tool_use one-liner derived from input
	Input      json.RawMessage `json:"input,omitempty"`       // tool_use raw input (object) — see CLIENT-SIDE CONTRACT godoc
	Output     string          `json:"output,omitempty"`      // tool_result content
	Status     string          `json:"status,omitempty"`      // tool_result: "ok" | "error"
	DurationMS int64           `json:"duration_ms,omitempty"` // tool_result duration if available
}

// claudeJSONLEvent is the partial schema we care about. Fields we don't
// use are decoded into RawMessage so a future field addition by the CLI
// doesn't break parsing.
type claudeJSONLEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Timestamp string          `json:"timestamp"`
	UUID      string          `json:"uuid"`
	Message   json.RawMessage `json:"message"`
	// tool_result events sometimes appear at top level under
	// "toolUseResult" instead of inside a content block (varies by
	// CLI version). We tolerate both shapes.
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

// claudeMessage is the inner "message" field. Only role + content +
// usage matter to us.
type claudeMessage struct {
	Role    string              `json:"role"`
	Content json.RawMessage     `json:"content"` // string OR []contentBlock
	Usage   *claudeMessageUsage `json:"usage,omitempty"`
}
type claudeMessageUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// claudeContentBlock is one entry in an assistant message's content
// array. The CLI emits these for text / tool_use / tool_result /
// thinking. We surface the first three.
type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`        // type=text
	ID        string          `json:"id,omitempty"`          // type=tool_use
	Name      string          `json:"name,omitempty"`        // type=tool_use
	Input     json.RawMessage `json:"input,omitempty"`       // type=tool_use
	ToolUseID string          `json:"tool_use_id,omitempty"` // type=tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // type=tool_result (string OR array)
	IsError   bool            `json:"is_error,omitempty"`    // type=tool_result
}

// toolInputProbe is the partial schema summariseToolInput decodes into to
// pick a one-liner label (Bash → command, Read/Write/Edit → file_path, …).
// A typed struct avoids the reflection + map cost of `map[string]any` per
// transcript line; unrecognised keys are skipped by encoding/json (#1010).
type toolInputProbe struct {
	Command  string `json:"command,omitempty"`
	FilePath string `json:"file_path,omitempty"`
	Path     string `json:"path,omitempty"`
	URL      string `json:"url,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
	Query    string `json:"query,omitempty"`
}
