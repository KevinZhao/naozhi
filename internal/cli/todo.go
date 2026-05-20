package cli

import (
	"encoding/json"
	"strconv"
	"strings"
)

// TodoItem mirrors one entry in Claude Code's TodoWrite tool input.
type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"` // pending | in_progress | completed
	ActiveForm string `json:"activeForm,omitempty"`
}

type todoWriteInput struct {
	Todos []TodoItem `json:"todos"`
}

// todoWriteInputRaw mirrors todoWriteInput but keeps the inner todos array
// as its original JSON bytes so callers that need both the parsed slice
// (for summary counts) and the on-wire array string (for dashboard
// rendering) can avoid the extra Marshal+copy that the old TodosDetailJSON
// helper paid on every TodoWrite event. R226-PERF-8.
type todoWriteInputRaw struct {
	Todos json.RawMessage `json:"todos"`
}

// ParseTodos extracts the todos array from a TodoWrite tool_use input.
// Returns (todos, true) on success, (nil, false) when input is malformed
// or the todos field is missing/empty.
//
// Equivalent to discarding the rawTodos return of ParseTodosWithRaw;
// kept for callers that only need the typed slice.
func ParseTodos(input json.RawMessage) ([]TodoItem, bool) {
	todos, _, ok := ParseTodosWithRaw(input)
	return todos, ok
}

// ParseTodosWithRaw extracts the todos array from a TodoWrite tool_use
// input and additionally returns the original JSON bytes of the todos
// field (a JSON array literal). Callers that need a "stable JSON
// representation" for the dashboard (the historical TodosDetailJSON
// payload) can use rawTodos directly instead of re-marshalling the
// parsed slice.
//
// Returns (nil, nil, false) when input is malformed or the todos field
// is missing/empty.
//
// rawTodos is borrowed from input — callers that retain it past the
// lifetime of input must copy it.
func ParseTodosWithRaw(input json.RawMessage) (todos []TodoItem, rawTodos json.RawMessage, ok bool) {
	if len(input) == 0 {
		return nil, nil, false
	}
	// Single Unmarshal pass into a parallel struct that keeps the todos
	// array as RawMessage. Then a second Unmarshal of just that
	// RawMessage into the typed slice — still cheaper than the prior
	// "full Unmarshal then Marshal of the resulting slice" pattern,
	// because only the leaves are decoded once and the outer JSON envelope
	// (the `{"todos":...}` wrapper) is never re-encoded.
	var rawW todoWriteInputRaw
	if err := json.Unmarshal(input, &rawW); err != nil {
		return nil, nil, false
	}
	if len(rawW.Todos) == 0 || string(rawW.Todos) == "null" {
		return nil, nil, false
	}
	if err := json.Unmarshal(rawW.Todos, &todos); err != nil {
		return nil, nil, false
	}
	if len(todos) == 0 {
		return nil, nil, false
	}
	return todos, rawW.Todos, true
}

// TodosDetailJSON returns a stable JSON representation of the todos list so
// downstream consumers (dashboard UI) can parse without needing access to the
// original tool input. Returns "" on marshal error.
//
// Prefer ParseTodosWithRaw + string(rawTodos) when the original tool_use
// input is still in scope: it skips the redundant Marshal that this
// helper performs.
func TodosDetailJSON(todos []TodoItem) string {
	b, err := json.Marshal(todos)
	if err != nil {
		return ""
	}
	return string(b)
}

// Status emoji used by TodosSummary / TodosMarkdown. Centralised so a
// future policy switch (e.g. ASCII-only deployment, alternative status
// glyphs) can land in one place. R227-CR-13.
//
// Byte-boundary safety: every glyph below is a multi-byte UTF-8 sequence
// (📋 / ✅ are 4 bytes, ▶ / ☐ are 3 bytes). Downstream consumers that
// truncate by byte length (IM message body caps, dashboard chip width)
// MUST cut on a rune boundary — not a byte boundary — or the trailing
// glyph will surface as a U+FFFD replacement char. textutil.TruncateRunes
// already enforces rune-boundary cuts; new consumers should route through
// it instead of slicing s[:N].
const (
	todoStatusEmojiSummary = "📋" // overall list header
	todoStatusEmojiDone    = "✅" // completed
	todoStatusEmojiActive  = "▶" // in_progress
	todoStatusEmojiPending = "☐" // pending / unknown-empty status
	todoStatusEmojiUnknown = "?" // future status values not yet recognised
	todoStatusFieldSep     = " · "
)

// TodosSummary returns a compact one-line overview suitable for the event
// summary field, e.g. "📋 5项 · ✅2 ▶1 ☐2".
// Unknown statuses (e.g. future values Claude Code may emit) are counted
// separately and surfaced with "?N" so silent miscategorisation doesn't hide
// state changes from the UI.
//
// The output uses multi-byte emoji glyphs (📋 ✅ ▶ ☐). Callers that need
// to truncate the result for display MUST cut on a rune boundary — see the
// emoji-constants block above for the byte-boundary safety contract. R227-CR-13.
func TodosSummary(todos []TodoItem) string {
	var done, active, pending, unknown int
	for _, t := range todos {
		switch t.Status {
		case "completed":
			done++
		case "in_progress":
			active++
		case "pending", "":
			pending++
		default:
			unknown++
		}
	}
	var b strings.Builder
	b.Grow(40)
	b.WriteString(todoStatusEmojiSummary)
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(len(todos)))
	b.WriteString("项")
	if done > 0 {
		b.WriteString(todoStatusFieldSep)
		b.WriteString(todoStatusEmojiDone)
		b.WriteString(strconv.Itoa(done))
	}
	if active > 0 {
		b.WriteString(todoStatusFieldSep)
		b.WriteString(todoStatusEmojiActive)
		b.WriteString(strconv.Itoa(active))
	}
	if pending > 0 {
		b.WriteString(todoStatusFieldSep)
		b.WriteString(todoStatusEmojiPending)
		b.WriteString(strconv.Itoa(pending))
	}
	if unknown > 0 {
		b.WriteString(todoStatusFieldSep)
		b.WriteString(todoStatusEmojiUnknown)
		b.WriteString(strconv.Itoa(unknown))
	}
	return b.String()
}

// TodosMarkdown renders the list for IM display. Uses the activeForm for
// in-progress items when available so users see "正在执行X" instead of "X".
// The capacity estimate uses actual byte lengths (CJK content is 3 bytes per
// rune) so the initial Builder grow is a single allocation.
func TodosMarkdown(todos []TodoItem) string {
	if len(todos) == 0 {
		return ""
	}
	// 16B header + per-line prefix (~6B for "✅ " / "▶ " / "☐ " / "? ")
	// + each content's actual byte length + 1 separator.
	est := 16
	for _, t := range todos {
		est += 8 + len(t.Content)
		if t.ActiveForm != "" && t.Status == "in_progress" {
			est += len(t.ActiveForm)
		}
	}
	var b strings.Builder
	b.Grow(est)
	b.WriteString("📋 任务清单\n")
	for i, t := range todos {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch t.Status {
		case "completed":
			b.WriteString("✅ ")
			b.WriteString(t.Content)
		case "in_progress":
			b.WriteString("▶ ")
			if t.ActiveForm != "" {
				b.WriteString(t.ActiveForm)
			} else {
				b.WriteString(t.Content)
			}
		case "pending", "":
			b.WriteString("☐ ")
			b.WriteString(t.Content)
		default:
			// Unknown status — render distinctly so operators notice a new
			// status value instead of silently conflating it with pending.
			b.WriteString("? ")
			b.WriteString(t.Content)
		}
	}
	return b.String()
}
