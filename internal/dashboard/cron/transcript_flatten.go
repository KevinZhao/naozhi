package cron

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// truncatedToolInputPlaceholder is the JSON value substituted for
// tool_use.Input fields that exceed maxToolInputBytes. Pre-encoded so the
// hot path never re-marshals; must be a valid JSON value (a string
// literal here) so the wire shape stays consistent for dashboard JS.
var truncatedToolInputPlaceholder = json.RawMessage(`"[truncated]"`)

// flattenJSONLEvent decodes one JSONL line into 0..N transcript turns.
// Returns (turns, token deltas, tool-call delta, parsedAny); parsedAny is
// true when the event maps to at least one recognised turn shape — the
// caller uses it to decide whether to set fallback:"raw". Per-type helpers
// own their own (decode → walk → emit) sub-flow.
func flattenJSONLEvent(ev *claudefs.Line, ts int64, nextIdx int) ([]transcriptTurn, transcriptTokens, int, bool) {
	switch ev.Type {
	case "user":
		return flattenUserEvent(ev, ts, nextIdx)
	case "assistant":
		return flattenAssistantEvent(ev, ts, nextIdx)
	case "system":
		return flattenSystemEvent(ev, ts, nextIdx)
	}
	return nil, transcriptTokens{}, 0, false
}

// flattenUserEvent emits a "user" text turn (when content carries one) plus
// zero or more "tool_result" turns (when the user message wraps a
// content-block array — how Claude carries tool_result payloads back into the
// conversation). Two-pass: count tool_result blocks first, pre-size out
// exactly, and skip the allocation on lines that contribute no turns.
func flattenUserEvent(ev *claudefs.Line, ts int64, nextIdx int) ([]transcriptTurn, transcriptTokens, int, bool) {
	tok := transcriptTokens{}

	var msg claudeMessage
	if err := json.Unmarshal(ev.Message, &msg); err != nil {
		return nil, tok, 0, false
	}
	text, blocks := decodeStringOrBlocks(msg.Content)
	hasText := text != ""
	toolResultCount := 0
	for i := range blocks {
		if blocks[i].Type == "tool_result" {
			toolResultCount++
		}
	}
	totalTurns := toolResultCount
	if hasText {
		totalTurns++
	}
	if totalTurns == 0 {
		return nil, tok, 0, false
	}
	out := make([]transcriptTurn, 0, totalTurns)
	parsed := false
	if hasText {
		out = append(out, transcriptTurn{
			Index: nextIdx + len(out),
			Kind:  "user",
			TS:    ts,
			Text:  sanitizeWireText(truncateRunes(text, maxAssistantTextBytes)),
		})
		parsed = true
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		parsed = true
		outStr := toolResultText(b.Content)
		// ANSI escapes are rare in agent tool_result text; skip the regex
		// (NFA traversal of every byte) when the ESC byte 0x1b is absent,
		// which is the common case.
		if strings.IndexByte(outStr, 0x1b) >= 0 {
			outStr = ansiEscRe.ReplaceAllString(outStr, "")
		}
		outStr = sanitizeWireText(truncateRunes(outStr, maxToolOutputBytes))
		status := "ok"
		if b.IsError {
			status = "error"
		}
		out = append(out, transcriptTurn{
			Index:     nextIdx + len(out),
			Kind:      "tool_result",
			TS:        ts,
			ToolUseID: b.ToolUseID,
			Output:    outStr,
			Status:    status,
		})
	}
	return out, tok, 0, parsed
}

// flattenAssistantEvent emits a single aggregated "assistant" text turn
// (multiple text blocks in one message are merged with blank-line separators
// because they split awkwardly as separate timeline entries) followed by
// per-tool_use turns. Returns the token usage delta from msg.Usage. Two-pass:
// aggregate text + count tool_use blocks, then emit at final indices — no
// prepend, no reindex, O(1) slice allocation.
func flattenAssistantEvent(ev *claudefs.Line, ts int64, nextIdx int) ([]transcriptTurn, transcriptTokens, int, bool) {
	tok := transcriptTokens{}
	toolCalls := 0

	var msg claudeMessage
	if err := json.Unmarshal(ev.Message, &msg); err != nil {
		return nil, tok, 0, false
	}
	_, blocks := decodeStringOrBlocks(msg.Content)
	if msg.Usage != nil {
		tok.Input = msg.Usage.InputTokens
		tok.Output = msg.Usage.OutputTokens
	}
	// First pass: aggregate text blocks + count tool_use blocks so the output
	// slice can be pre-sized exactly.
	var textBuf strings.Builder
	toolUseCount := 0
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if textBuf.Len() > 0 {
				textBuf.WriteString("\n\n")
			}
			textBuf.WriteString(b.Text)
		case "tool_use":
			toolUseCount++
		}
	}
	hasText := textBuf.Len() > 0
	totalTurns := toolUseCount
	if hasText {
		totalTurns++
	}
	if totalTurns == 0 {
		return nil, tok, 0, false
	}
	out := make([]transcriptTurn, 0, totalTurns)
	parsed := false
	if hasText {
		out = append(out, transcriptTurn{
			Index:  nextIdx,
			Kind:   "assistant",
			TS:     ts,
			Text:   sanitizeWireText(truncateRunes(textBuf.String(), maxAssistantTextBytes)),
			Tokens: tok.Output,
		})
		parsed = true
	}
	// Second pass: emit tool_use turns in source order at indices that
	// follow the (optional) assistant turn. No reindex needed — indices
	// land in their final positions on first write.
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		toolCalls++
		summary := sanitizeWireText(summariseToolInput(b.Name, b.Input))
		// Cap the raw Input JSON we surface; summary was built from the original
		// bytes so the one-line label survives even when Input is replaced with
		// the [truncated] placeholder.
		input := b.Input
		if len(input) > maxToolInputBytes {
			input = truncatedToolInputPlaceholder
		}
		// json.RawMessage's `omitempty` only checks len==0, so a literal `null`
		// would survive as `"input": null` and confuse the dashboard's
		// "has tool input?" presence check. Normalise to a zero-length
		// RawMessage so omitempty trips (#822).
		if isJSONNull(input) {
			input = nil
		}
		// Redact secrets so credentials a cron job read into a tool call don't
		// leak verbatim (#1914).
		input = redactToolInput(input)
		out = append(out, transcriptTurn{
			Index:     nextIdx + len(out),
			Kind:      "tool_use",
			TS:        ts,
			Tool:      b.Name,
			ToolUseID: b.ID,
			Summary:   summary,
			Input:     input,
		})
		parsed = true
	}
	return out, tok, toolCalls, parsed
}

// flattenSystemEvent surfaces system error events (claude CLI lifecycle
// init / error). init events are dropped because they don't add
// timeline value; only `subtype == "error"` becomes an "error" turn.
// Unmarshal failures return early (consistent with sibling flatten helpers).
// The out slice is allocated lazily — only when an error turn is emitted.
func flattenSystemEvent(ev *claudefs.Line, ts int64, nextIdx int) ([]transcriptTurn, transcriptTokens, int, bool) {
	tok := transcriptTokens{}

	var sys struct {
		Subtype string `json:"subtype"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(ev.Message, &sys); err != nil {
		slog.Debug("cron transcript: system event unmarshal failed; skipping",
			"err", err)
		return nil, tok, 0, false
	}
	if sys.Subtype != "error" || sys.Message == "" {
		return nil, tok, 0, false
	}
	out := []transcriptTurn{{
		Index: nextIdx,
		Kind:  "error",
		TS:    ts,
		Text:  sanitizeWireText(truncateRunes(sys.Message, maxAssistantTextBytes)),
	}}
	return out, tok, 0, true
}

// summariseToolInput builds a one-line label for the tool_use card header.
// Best-effort: Bash → command, Read/Write/Edit → file_path, otherwise a
// JSON-trimmed dump of the input. Inputs above summariseInputCap are refused
// before json.Unmarshal so a deeply-nested blob cannot drive the parser just
// to populate a 200-byte label (#645).
func summariseToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	if len(input) > summariseInputCap {
		// Oversize input is opaque: the wire payload already carries the
		// [truncated] placeholder and the dashboard handles a missing summary.
		return ""
	}
	var probe toolInputProbe
	if err := json.Unmarshal(input, &probe); err != nil {
		return ""
	}
	// Priority order so callers see deterministic output when a tool
	// populates multiple fields.
	candidates := [...]string{
		probe.Command, probe.FilePath, probe.Path,
		probe.URL, probe.Pattern, probe.Query,
	}
	for _, s := range candidates {
		if s != "" {
			return textutil.RedactSecrets(osutil.SanitizeForLog(s, 200))
		}
	}
	// Fallback: reuse the original bytes (json.Unmarshal does not mutate its
	// source), no need to Marshal again.
	return textutil.RedactSecrets(osutil.SanitizeForLog(string(input), 200))
}

// isJSONNull reports whether b is the JSON `null` literal (with optional
// surrounding ASCII whitespace per RFC 8259). Used to suppress an upstream
// `"input": null` so the wire response honours the RawMessage `omitempty`
// contract (#822).
func isJSONNull(b json.RawMessage) bool {
	// RFC 8259 permits insignificant whitespace (sp/tab/lf/cr) outside
	// structural tokens; trim conservatively before the byte compare.
	for len(b) > 0 {
		switch b[0] {
		case ' ', '\t', '\n', '\r':
			b = b[1:]
		default:
			goto trail
		}
	}
trail:
	for len(b) > 0 {
		switch b[len(b)-1] {
		case ' ', '\t', '\n', '\r':
			b = b[:len(b)-1]
		default:
			goto compare
		}
	}
compare:
	return len(b) == 4 && b[0] == 'n' && b[1] == 'u' && b[2] == 'l' && b[3] == 'l'
}

// redactToolInput strips well-known secret tokens (API keys, passwords, …)
// out of the raw tool_use.Input JSON before it reaches the wire; the Summary
// is already redacted via sanitizeWireText but the full Input surfaced
// verbatim (#1914).
//
// The redaction MUST leave valid JSON behind: an `input` json.Encoder refuses
// to emit fails the whole response (WriteJSON → 500). RedactSecrets' `KEY=value`
// masking can swallow a closing `"}` inside a JSON string, so redactRawJSON
// falls back to per-string-value redaction whenever the text-level result is
// malformed. Clean input is returned unchanged (aliased).
func redactToolInput(in json.RawMessage) json.RawMessage {
	if len(in) == 0 {
		return in
	}
	out := redactRawJSON(string(in), textutil.RedactSecrets)
	if string(out) == string(in) {
		return in
	}
	return out
}
