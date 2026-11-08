package session

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/ctxutil"
)

// TestTitleSkipsCustodyMessages: a session the gateway keeps going under
// custody keeps the last prompt the user wrote as its title.
func TestTitleSkipsCustodyMessages(t *testing.T) {
	t.Parallel()
	custody := clievent.CustodyLead + "出错重试 · 第 1/8 次\n继续"
	ctx := context.Background()

	s := &ManagedSession{key: "dashboard:direct:a:general"}
	s.cachePrompt(ctx, "整理周报", nil)
	// A custody message is plain text now, known by its source; an older
	// one by its lead.
	s.cachePrompt(ctxutil.WithSource(ctx, clievent.SourceCustody), "继续", nil)
	s.cachePrompt(ctx, custody, nil)
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

// TestLastUserSent: only a message the user sent is theirs; custody, a
// relay and the Planner's dispatch are not.
func TestLastUserSent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &ManagedSession{key: "dashboard:direct:a:general"}
	for _, src := range []string{clievent.SourceCustody, clievent.SourceRelay, clievent.SourcePlanner} {
		s.cachePrompt(ctxutil.WithSource(ctx, src), "继续", nil)
	}
	if got := s.LastUserSent(); !got.IsZero() {
		t.Fatalf("after the gateway's messages LastUserSent = %v, want zero", got)
	}
	before := time.Now()
	s.cachePrompt(ctx, "整理周报", nil)
	if got := s.LastUserSent(); got.Before(before) {
		t.Errorf("after the user's message LastUserSent = %v, want ≥ %v", got, before)
	}
}
