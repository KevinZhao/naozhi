package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseTodos(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantOK  bool
	}{
		{"empty input", ``, 0, false},
		{"malformed", `{bad`, 0, false},
		{"missing todos", `{"other":1}`, 0, false},
		{"empty todos", `{"todos":[]}`, 0, false},
		{"two todos", `{"todos":[{"content":"a","status":"pending"},{"content":"b","status":"completed","activeForm":"doing b"}]}`, 2, true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseTodos(json.RawMessage(tc.input))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tc.wantLen)
			}
		})
	}
}

func TestTodosSummary(t *testing.T) {
	t.Parallel()
	todos := []TodoItem{
		{Content: "A", Status: "completed"},
		{Content: "B", Status: "in_progress"},
		{Content: "C", Status: "pending"},
		{Content: "D", Status: "pending"},
	}
	got := TodosSummary(todos)
	for _, want := range []string{"4项", "✅1", "▶1", "☐2"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
}

func TestTodosSummaryOnlyPending(t *testing.T) {
	t.Parallel()
	got := TodosSummary([]TodoItem{{Content: "A", Status: "pending"}})
	if strings.Contains(got, "✅") || strings.Contains(got, "▶") {
		t.Errorf("unexpected completed/active counter in %q", got)
	}
}

func TestTodosMarkdown(t *testing.T) {
	t.Parallel()
	todos := []TodoItem{
		{Content: "写代码", Status: "completed"},
		{Content: "跑测试", Status: "in_progress", ActiveForm: "正在跑测试"},
		{Content: "发 PR", Status: "pending"},
	}
	got := TodosMarkdown(todos)
	if !strings.HasPrefix(got, "📋 任务清单") {
		t.Errorf("missing header: %q", got)
	}
	for _, want := range []string{"✅ 写代码", "▶ 正在跑测试", "☐ 发 PR"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing line %q in output:\n%s", want, got)
		}
	}
}

func TestTodosMarkdownEmpty(t *testing.T) {
	t.Parallel()
	if got := TodosMarkdown(nil); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestTodosDetailJSONRoundTrip(t *testing.T) {
	t.Parallel()
	in := []TodoItem{{Content: "x", Status: "pending"}}
	s := TodosDetailJSON(in)
	var out []TodoItem
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 1 || out[0].Content != "x" {
		t.Errorf("round-trip mismatch: %+v", out)
	}
}

// TestParseTodosWithRaw_RawIsArrayLiteral pins the R226-PERF-8 contract:
// rawTodos must come back as the original `[...]` array bytes — not the
// `{"todos":[...]}` envelope and not a re-serialised slice. process_event_
// format.go assigns rawTodos straight into entry.Detail and the dashboard
// renderer relies on that being a JSON array literal.
func TestParseTodosWithRaw_RawIsArrayLiteral(t *testing.T) {
	t.Parallel()
	in := json.RawMessage(`{"todos":[{"content":"a","status":"pending"},{"content":"b","status":"completed"}]}`)
	todos, raw, ok := ParseTodosWithRaw(in)
	if !ok {
		t.Fatal("ParseTodosWithRaw returned ok=false on valid input")
	}
	if len(todos) != 2 || todos[0].Content != "a" || todos[1].Status != "completed" {
		t.Fatalf("unexpected parsed todos: %+v", todos)
	}
	rawStr := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(rawStr, "[") || !strings.HasSuffix(rawStr, "]") {
		t.Fatalf("raw must be a JSON array literal, got %q", rawStr)
	}
	// raw must round-trip back into a []TodoItem (i.e. be Array.isArray
	// from the dashboard's perspective).
	var out []TodoItem
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("raw todos must unmarshal as []TodoItem, got %q: %v", rawStr, err)
	}
	if len(out) != 2 {
		t.Fatalf("round-trip len mismatch: %d", len(out))
	}
}

// TestParseTodosWithRaw_RejectsMalformed mirrors ParseTodos's error paths.
func TestParseTodosWithRaw_RejectsMalformed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ``},
		{"malformed", `{bad`},
		{"missing todos", `{"other":1}`},
		{"empty todos", `{"todos":[]}`},
		{"null todos", `{"todos":null}`},
		{"todos not array", `{"todos":42}`},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			todos, raw, ok := ParseTodosWithRaw(json.RawMessage(tc.input))
			if ok {
				t.Fatalf("expected ok=false for %q", tc.input)
			}
			if todos != nil || raw != nil {
				t.Fatalf("expected nil returns for malformed input, got todos=%v raw=%q", todos, string(raw))
			}
		})
	}
}
