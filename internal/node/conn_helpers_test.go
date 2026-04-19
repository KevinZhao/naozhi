package node

import (
	"sync"
	"testing"
)

// mockSink is a minimal EventSink for testing subscription helpers.
// It is safe for concurrent use: SendJSON and SendRaw may be called from relay goroutines.
type mockSink struct {
	id int
	mu sync.Mutex

	jsonMsgs []any
	rawMsgs  [][]byte
}

func (m *mockSink) SendJSON(v any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jsonMsgs = append(m.jsonMsgs, v)
}

func (m *mockSink) SendRaw(data []byte) {
	cp := make([]byte, len(data))
	copy(cp, data)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rawMsgs = append(m.rawMsgs, cp)
}

// JSONMsgs returns a snapshot of received JSON messages.
func (m *mockSink) JSONMsgs() []interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]interface{}, len(m.jsonMsgs))
	copy(cp, m.jsonMsgs)
	return cp
}

// RawMsgs returns a snapshot of received raw messages.
func (m *mockSink) RawMsgs() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([][]byte, len(m.rawMsgs))
	copy(cp, m.rawMsgs)
	return cp
}

// RawMsgCount returns the number of raw messages received.
func (m *mockSink) RawMsgCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rawMsgs)
}

// JSONMsgCount returns the number of JSON messages received.
func (m *mockSink) JSONMsgCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.jsonMsgs)
}

func TestRemoveSub_basic(t *testing.T) {
	s1 := &mockSink{id: 1}
	s2 := &mockSink{id: 2}
	subs := map[string][]EventSink{
		"key1": {s1, s2},
	}

	empty := removeSub(subs, "key1", s1)
	if empty {
		t.Fatal("want non-empty after removing one of two")
	}
	if len(subs["key1"]) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(subs["key1"]))
	}
	if subs["key1"][0] != s2 {
		t.Fatal("remaining sink should be s2")
	}
}

func TestRemoveSub_lastRemovalDeletesKey(t *testing.T) {
	s1 := &mockSink{id: 1}
	subs := map[string][]EventSink{
		"key1": {s1},
	}
	empty := removeSub(subs, "key1", s1)
	if !empty {
		t.Fatal("want empty=true after removing last subscriber")
	}
	if _, ok := subs["key1"]; ok {
		t.Fatal("key should be deleted from map")
	}
}

func TestRemoveSub_nonExistentSinkIsNoop(t *testing.T) {
	s1 := &mockSink{id: 1}
	s2 := &mockSink{id: 2}
	subs := map[string][]EventSink{
		"key1": {s1},
	}
	empty := removeSub(subs, "key1", s2) // s2 is not subscribed
	// s1 still there so not empty
	if empty {
		t.Fatal("want empty=false when sink not found")
	}
	if len(subs["key1"]) != 1 {
		t.Fatalf("expected 1 subscriber, got %d", len(subs["key1"]))
	}
}

func TestRemoveSub_missingKeyIsNoop(t *testing.T) {
	s1 := &mockSink{id: 1}
	subs := map[string][]EventSink{}
	empty := removeSub(subs, "no-such-key", s1)
	if !empty {
		t.Fatal("want empty=true for absent key")
	}
}

func TestRemoveSubAll_removesFromMultipleKeys(t *testing.T) {
	s1 := &mockSink{id: 1}
	s2 := &mockSink{id: 2}
	subs := map[string][]EventSink{
		"key1": {s1, s2},
		"key2": {s1},
		"key3": {s2},
	}

	emptyKeys := removeSubAll(subs, s1)

	// s1 should be gone from key1 and key2; key2 should be deleted
	for _, k := range emptyKeys {
		if _, ok := subs[k]; ok {
			t.Errorf("key %q should have been removed from map", k)
		}
	}
	// key1 still has s2
	if len(subs["key1"]) != 1 || subs["key1"][0] != s2 {
		t.Errorf("key1 should still have s2, got %v", subs["key1"])
	}
	// key3 still has s2 (unaffected)
	if len(subs["key3"]) != 1 || subs["key3"][0] != s2 {
		t.Errorf("key3 should still have s2, got %v", subs["key3"])
	}
	// key2 was empty after removal, so it should be in emptyKeys
	found := false
	for _, k := range emptyKeys {
		if k == "key2" {
			found = true
		}
	}
	if !found {
		t.Error("key2 should appear in emptyKeys")
	}
}

func TestRemoveSubAll_noSubscriptionsIsNoop(t *testing.T) {
	s1 := &mockSink{id: 1}
	subs := map[string][]EventSink{
		"key1": {&mockSink{id: 2}},
	}
	emptyKeys := removeSubAll(subs, s1)
	if len(emptyKeys) != 0 {
		t.Errorf("expected no empty keys, got %v", emptyKeys)
	}
	if len(subs["key1"]) != 1 {
		t.Error("key1 should be untouched")
	}
}
