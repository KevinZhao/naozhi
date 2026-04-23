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
