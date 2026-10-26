package node

import (
	"encoding/json"
	"sync"
)

// mockSink is a minimal EventSink shared by the node package tests.
// It is safe for concurrent use: SendJSON and SendRaw may be called from relay goroutines.
type mockSink struct {
	id int
	mu sync.Mutex

	jsonMsgs []any
	rawMsgs  [][]byte
}

func (m *mockSink) SendJSON(v any) {
	// Normalise every frame into the legacy ServerMsg decode view: production
	// code sends per-type wsproto frames (#2535), and the assertions here
	// type-assert `.(ServerMsg)` — round-tripping through JSON keeps them
	// working against whatever struct the sender used, exactly like the
	// browser's JSON.parse does.
	if data, err := json.Marshal(v); err == nil {
		var msg ServerMsg
		if json.Unmarshal(data, &msg) == nil && msg.Type != "" {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.jsonMsgs = append(m.jsonMsgs, msg)
			return
		}
	}
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
