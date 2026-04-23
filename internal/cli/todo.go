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

// ParseTodos extracts the todos array from a TodoWrite tool_use input.
// Returns (todos, true) on success, (nil, false) when input is malformed
// or the todos field is missing/empty.
func ParseTodos(input json.RawMessage) ([]TodoItem, bool) {
	if len(input) == 0 {
		return nil, false
	}
	var w todoWriteInput
	if err := json.Unmarshal(input, &w); err != nil {
		return nil, false
	}
	if len(w.Todos) == 0 {
		return nil, false
	}
	return w.Todos, true
}

// TodosDetailJSON returns a stable JSON representation of the todos list so
// downstream consumers (dashboard UI) can parse without needing access to the
// original tool input. Returns "" on marshal error.
func TodosDetailJSON(todos []TodoItem) string {
	b, err := json.Marshal(todos)
	if err != nil {
		return ""
	}
	return string(b)
}

// TodosSummary returns a compact one-line overview suitable for the event
// summary field, e.g. "📋 5项 · ✅2 ▶1 ☐2".
func TodosSummary(todos []TodoItem) string {
	var done, active, pending int
	for _, t := range todos {
		switch t.Status {
		case "completed":
			done++
		case "in_progress":
			active++
		default:
			pending++
		}
	}
	var b strings.Builder
	b.Grow(32)
	b.WriteString("📋 ")
	b.WriteString(strconv.Itoa(len(todos)))
	b.WriteString("项")
	if done > 0 {
		b.WriteString(" · ✅")
		b.WriteString(strconv.Itoa(done))
	}
	if active > 0 {
		b.WriteString(" · ▶")
		b.WriteString(strconv.Itoa(active))
	}
	if pending > 0 {
		b.WriteString(" · ☐")
		b.WriteString(strconv.Itoa(pending))
	}
	return b.String()
}

// TodosMarkdown renders the list for IM display. Uses the activeForm for
// in-progress items when available so users see "正在执行X" instead of "X".
func TodosMarkdown(todos []TodoItem) string {
	if len(todos) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(todos) * 40)
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
		default:
			b.WriteString("☐ ")
			b.WriteString(t.Content)
		}
	}
	return b.String()
}
