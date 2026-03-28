package cli

import (
	"sync"
	"testing"
	"time"
)

func TestNewEventLog_DefaultSize(t *testing.T) {
	l := NewEventLog(0)
	if l.maxSize != defaultEventLogSize {
		t.Errorf("maxSize = %d, want %d", l.maxSize, defaultEventLogSize)
	}
}

func TestNewEventLog_CustomSize(t *testing.T) {
	l := NewEventLog(50)
	if l.maxSize != 50 {
		t.Errorf("maxSize = %d, want 50", l.maxSize)
	}
}

func TestEventLog_Append_And_Entries(t *testing.T) {
	l := NewEventLog(10)
	l.Append(EventEntry{Time: 1000, Type: "thinking", Summary: "hello"})
	l.Append(EventEntry{Time: 2000, Type: "tool_use", Summary: "Read"})

	entries := l.Entries()
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].Type != "thinking" || entries[1].Type != "tool_use" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestEventLog_Append_AutoTimestamp(t *testing.T) {
	l := NewEventLog(10)
	l.Append(EventEntry{Type: "system"})
	entries := l.Entries()
	if entries[0].Time == 0 {
		t.Error("expected auto-assigned timestamp")
	}
}

func TestEventLog_Append_Overflow(t *testing.T) {
	l := NewEventLog(10)
	for i := 0; i < 20; i++ {
		l.Append(EventEntry{Time: int64(i + 1), Type: "test"})
	}
	entries := l.Entries()
	if len(entries) > 10 {
		t.Errorf("len = %d, should be <= 10", len(entries))
	}
	// Earliest surviving entry must be > 0 (entry 0 was dropped)
	if entries[0].Time <= 1 {
		t.Errorf("oldest entry Time = %d, expected > 1 (old entries should be dropped)", entries[0].Time)
	}
}

func TestEventLog_EntriesSince(t *testing.T) {
	l := NewEventLog(100)
	l.Append(EventEntry{Time: 1000, Type: "a"})
	l.Append(EventEntry{Time: 2000, Type: "b"})
	l.Append(EventEntry{Time: 3000, Type: "c"})

	entries := l.EntriesSince(1500)
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].Type != "b" || entries[1].Type != "c" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestEventLog_EntriesSince_NoMatch(t *testing.T) {
	l := NewEventLog(100)
	l.Append(EventEntry{Time: 1000, Type: "a"})
	entries := l.EntriesSince(2000)
	if len(entries) != 0 {
		t.Errorf("len = %d, want 0", len(entries))
	}
}

func TestEventLog_Entries_IsCopy(t *testing.T) {
	l := NewEventLog(10)
	l.Append(EventEntry{Time: 1000, Type: "a"})
	entries := l.Entries()
	entries[0].Type = "modified"

	original := l.Entries()
	if original[0].Type != "a" {
		t.Error("Entries() should return a copy, not a reference")
	}
}

func TestTruncateRunes_Short(t *testing.T) {
	got := TruncateRunes("hello", 10)
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestTruncateRunes_Truncated(t *testing.T) {
	got := TruncateRunes("hello world", 5)
	if got != "hello..." {
		t.Errorf("got %q, want %q", got, "hello...")
	}
}

func TestTruncateRunes_Unicode(t *testing.T) {
	got := TruncateRunes("你好世界测试", 4)
	if got != "你好世界..." {
		t.Errorf("got %q, want %q", got, "你好世界...")
	}
}

// ─── Subscribe tests ─────────────────────────────────────────────────────────

func TestEventLog_Subscribe_Notified(t *testing.T) {
	l := NewEventLog(100)
	ch, unsub := l.Subscribe()
	defer unsub()

	l.Append(EventEntry{Time: 1000, Type: "test"})

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Error("subscriber should be notified on Append")
	}
}

func TestEventLog_Subscribe_NonBlockingWhenFull(t *testing.T) {
	l := NewEventLog(100)
	ch, unsub := l.Subscribe()
	defer unsub()

	// Fill the buffered(1) channel
	l.Append(EventEntry{Time: 1000, Type: "a"})
	<-ch

	// Fill the channel again
	l.Append(EventEntry{Time: 2000, Type: "b"})

	// Append again without draining: must not block
	done := make(chan struct{})
	go func() {
		l.Append(EventEntry{Time: 3000, Type: "c"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Append should not block when subscriber channel is full")
	}
}

func TestEventLog_Subscribe_MultipleSubscribers(t *testing.T) {
	l := NewEventLog(100)
	ch1, unsub1 := l.Subscribe()
	defer unsub1()
	ch2, unsub2 := l.Subscribe()
	defer unsub2()

	l.Append(EventEntry{Time: 1000, Type: "test"})

	for i, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Errorf("subscriber %d should be notified", i)
		}
	}
}

func TestEventLog_Unsubscribe_Cleanup(t *testing.T) {
	l := NewEventLog(100)
	_, unsub := l.Subscribe()

	l.subMu.Lock()
	count := len(l.subscribers)
	l.subMu.Unlock()
	if count != 1 {
		t.Fatalf("subscribers = %d, want 1", count)
	}

	unsub()

	l.subMu.Lock()
	count = len(l.subscribers)
	l.subMu.Unlock()
	if count != 0 {
		t.Errorf("subscribers after unsub = %d, want 0", count)
	}
}

func TestEventLog_Unsubscribe_Idempotent(t *testing.T) {
	l := NewEventLog(100)
	_, unsub := l.Subscribe()
	unsub()
	unsub() // should not panic
}

func TestEventLog_Subscribe_ConcurrentSafe(t *testing.T) {
	l := NewEventLog(100)
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, unsub := l.Subscribe()
			l.Append(EventEntry{Time: time.Now().UnixMilli(), Type: "concurrent"})
			select {
			case <-ch:
			case <-time.After(time.Second):
			}
			unsub()
		}()
	}
	wg.Wait()
}

func TestEventLog_DetailAndToolFields(t *testing.T) {
	l := NewEventLog(10)
	l.Append(EventEntry{
		Time:   1000,
		Type:   "tool_use",
		Tool:   "Read",
		Detail: "Read: /path/to/file",
	})
	entries := l.Entries()
	if entries[0].Tool != "Read" {
		t.Errorf("Tool = %q, want Read", entries[0].Tool)
	}
	if entries[0].Detail != "Read: /path/to/file" {
		t.Errorf("Detail = %q, want Read: /path/to/file", entries[0].Detail)
	}
}
