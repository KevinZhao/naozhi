package cli

import "testing"

// TestEventLog_LastResponseSummary_StripsMarkdown pins #2435 item 1: the
// sidebar second line is rendered as plain text, so the stored response
// preview must not carry heading / emphasis / code notation. Both the live
// Append path and the InjectHistory AppendBatch path are covered.
func TestEventLog_LastResponseSummary_StripsMarkdown(t *testing.T) {
	t.Parallel()

	l := NewEventLog(10)
	l.Append(EventEntry{Type: "text", Summary: "## 判分\n\n**≈129.5/150**，提交 `b466411`"})
	if got, want := l.LastResponseSummary(), "判分 ≈129.5/150，提交 b466411"; got != want {
		t.Errorf("Append: LastResponseSummary = %q, want %q", got, want)
	}

	b := NewEventLog(10)
	b.AppendBatch([]EventEntry{
		{Type: "user", Summary: "q"},
		{Type: "text", Summary: "- 第一项\n- [链接](https://x.example)"},
	})
	if got, want := b.LastResponseSummary(), "第一项 链接"; got != want {
		t.Errorf("AppendBatch: LastResponseSummary = %q, want %q", got, want)
	}
}
