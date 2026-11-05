package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestTitleSkipsCustodyMessages: a session the gateway keeps going under
// custody keeps the last prompt the user wrote as its title.
func TestTitleSkipsCustodyMessages(t *testing.T) {
	t.Parallel()
	custody := clievent.CustodyLead + "出错重试 · 第 1/8 次\n继续"

	s := &ManagedSession{key: "dashboard:direct:a:general"}
	s.cachePrompt("整理周报", nil)
	s.cachePrompt(custody, nil)
	if got := loadAtomicString(&s.lastPrompt); got != "整理周报" {
		t.Fatalf("live title = %q, want the user's prompt", got)
	}

	for _, tc := range []struct {
		name  string
		entry clievent.EventEntry
	}{
		{"by source", clievent.EventEntry{Type: clievent.KindUser, Summary: "继续", Source: clievent.SourceCustody}},
		{"by lead", clievent.EventEntry{Type: clievent.KindUser, Summary: custody}},
	} {
		prompt, _, _ := scanLastSummaries([]clievent.EventEntry{
			{Type: clievent.KindUser, Summary: "整理周报"},
			tc.entry,
		})
		if prompt != "整理周报" {
			t.Errorf("%s: replayed title = %q, want the user's prompt", tc.name, prompt)
		}
	}
}
