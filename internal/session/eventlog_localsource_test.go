package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/history/merged"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
)

// Tests for the eventlog_bridge.go history-source constructors that
// consolidate the naozhilog/merged construction out of router_core.go
// and router_lifecycle.go (#403, #567). Behaviour must match the
// previous inline construction exactly:
//   - eventLogDir == "" → fallback returned unchanged (event log opt-out)
//   - eventLogDir != "" → merged.Source{Local: naozhilog, Fallback}
//   - nil fallback → history.Noop (never-nil contract)

func TestNewEventLogLocalSource(t *testing.T) {
	src := newEventLogLocalSource("/tmp/events", "feishu:dm:u1")
	if src == nil {
		t.Fatal("newEventLogLocalSource returned nil")
	}
	// Must be the naozhilog concrete type so the tier-1 LoadLatest call
	// site (router_core background loader) keeps compiling against it.
	if _, ok := any(src).(*naozhilog.Source); !ok {
		t.Fatalf("expected *naozhilog.Source, got %T", src)
	}
}

func TestMergeWithEventLog_OptOutReturnsFallback(t *testing.T) {
	fallback := history.Noop{}
	got := mergeWithEventLog("", "k", fallback)
	// Empty eventLogDir → fallback returned unchanged, NOT wrapped in merged.
	if _, isMerged := got.(*merged.Source); isMerged {
		t.Fatal("empty eventLogDir must not wrap fallback in merged.Source")
	}
	if _, ok := got.(history.Noop); !ok {
		t.Fatalf("expected fallback (history.Noop) returned unchanged, got %T", got)
	}
}

func TestMergeWithEventLog_WrapsWhenDirSet(t *testing.T) {
	fallback := history.Noop{}
	got := mergeWithEventLog("/tmp/events", "k", fallback)
	m, ok := got.(*merged.Source)
	if !ok {
		t.Fatalf("expected *merged.Source, got %T", got)
	}
	if m.Local == nil {
		t.Fatal("merged.Source.Local must be the naozhilog event-log tier")
	}
	if _, ok := m.Local.(*naozhilog.Source); !ok {
		t.Fatalf("expected Local to be *naozhilog.Source, got %T", m.Local)
	}
	if m.Fallback == nil {
		t.Fatal("merged.Source.Fallback must be preserved")
	}
}

func TestMergeWithEventLog_NilFallbackBecomesNoop(t *testing.T) {
	// Opt-out path with nil fallback must still return a non-nil source
	// (the attachHistorySource never-nil contract).
	got := mergeWithEventLog("", "k", nil)
	if got == nil {
		t.Fatal("nil fallback with empty dir must return history.Noop, not nil")
	}
	if _, ok := got.(history.Noop); !ok {
		t.Fatalf("expected history.Noop for nil fallback, got %T", got)
	}

	// Wrapped path with nil fallback must compose Noop under merged.
	gotWrapped := mergeWithEventLog("/tmp/events", "k", nil)
	m, ok := gotWrapped.(*merged.Source)
	if !ok {
		t.Fatalf("expected *merged.Source, got %T", gotWrapped)
	}
	if m.Fallback == nil {
		t.Fatal("nil fallback must be substituted with history.Noop, not left nil")
	}
}
