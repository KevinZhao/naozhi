package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
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

// TestTodoStatusEmojiConstants_RuneBoundary pins the R227-CR-13 contract:
// the emoji constants TodosSummary writes to its output are valid UTF-8
// rune sequences, so consumers slicing on a rune boundary (textutil.TruncateRunes
// / utf8.DecodeRuneInString) can never land inside a multi-byte glyph.
//
// The test also fences the byte-length expectations: a downstream consumer
// that wrongly truncates by len(byte) would surface U+FFFD if the cut fell
// mid-glyph. Locking the per-emoji byte counts here lets reviewers catch a
// change to the emoji table that would silently break IM message-byte caps
// the next time a 4-byte emoji is replaced with a 3-byte one (or vice versa).
func TestTodoStatusEmojiConstants_RuneBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		got      string
		wantSize int // bytes per glyph
	}{
		{"summary 📋", todoStatusEmojiSummary, 4},
		{"done ✅", todoStatusEmojiDone, 3},
		{"active ▶", todoStatusEmojiActive, 3},
		{"pending ☐", todoStatusEmojiPending, 3},
		{"unknown ?", todoStatusEmojiUnknown, 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !utf8.ValidString(tc.got) {
				t.Fatalf("%s: not valid UTF-8: %q", tc.name, tc.got)
			}
			if utf8.RuneCountInString(tc.got) != 1 {
				t.Fatalf("%s: must be a single rune for clean truncation, got %d runes",
					tc.name, utf8.RuneCountInString(tc.got))
			}
			if got := len(tc.got); got != tc.wantSize {
				t.Errorf("%s: byte size = %d, want %d (any change here may break downstream byte-cap consumers — see godoc)",
					tc.name, got, tc.wantSize)
			}
		})
	}
}

// TestTodosSummary_RuneBoundarySafe pins the R227-CR-13 cross-cutting
// contract: every byte position in TodosSummary's output sits on a UTF-8
// rune boundary, so caller-side rune-aware truncation can never slice mid
// glyph. utf8.DecodeRuneInString returns RuneError + size 1 for an invalid
// start byte; we walk the entire output to assert no such position exists.
func TestTodosSummary_RuneBoundarySafe(t *testing.T) {
	t.Parallel()
	got := TodosSummary([]TodoItem{
		{Content: "A", Status: "completed"},
		{Content: "B", Status: "in_progress"},
		{Content: "C", Status: "pending"},
		{Content: "D", Status: "weird-future-status"},
	})
	if !utf8.ValidString(got) {
		t.Fatalf("output is not valid UTF-8: %q", got)
	}
	// All four counters should appear so the tested string actually
	// exercises every emoji branch.
	for _, want := range []string{
		todoStatusEmojiSummary,
		todoStatusEmojiDone,
		todoStatusEmojiActive,
		todoStatusEmojiPending,
		todoStatusEmojiUnknown,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing emoji %q (one branch did not fire)", got, want)
		}
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
