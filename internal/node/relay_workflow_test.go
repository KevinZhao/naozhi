package node

import (
	"sync"
	"testing"
)

type recordingSink struct {
	mu  sync.Mutex
	raw []string
}

func (s *recordingSink) SendJSON(any) {}

func (s *recordingSink) SendRaw(b []byte) {
	s.mu.Lock()
	s.raw = append(s.raw, string(b))
	s.mu.Unlock()
}

// A remote node's workflow frames are not relayed to the browser (NG3); its
// other frames are.
func TestForwardEvent_DropsWorkflowFrames(t *testing.T) {
	r := newWSRelay(&HTTPClient{ID: "n1"})
	sink := &recordingSink{}
	const key = "feishu:p2p:u1"
	r.mu.Lock()
	r.book.subs[key] = []EventSink{sink}
	r.mu.Unlock()

	r.forwardEvent([]byte(`{"type":"workflow_set","key":"feishu:p2p:u1","epoch":"e","task_ids":[],"server_now":1}`))
	r.forwardEvent([]byte(`{"type":"workflow_state","key":"feishu:p2p:u1","task_id":"w1","epoch":"e","version":1,"full":true,"server_now":1,"workflow":{}}`))
	r.forwardEvent([]byte(`{"type":"event","key":"feishu:p2p:u1","event":{"time":1}}`))

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.raw) != 1 || sink.raw[0] != `{"node":"n1","type":"event","key":"feishu:p2p:u1","event":{"time":1}}` {
		t.Fatalf("relayed %q, want only the event frame", sink.raw)
	}
}
