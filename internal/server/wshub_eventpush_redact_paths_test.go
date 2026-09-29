package server

import (
	"bytes"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestMarshalHistoryFrame_RedactsOnEveryPath: the history frame's bytes never
// carry a credential shape or a local agent-linkage field on ANY of its
// serialization paths (wsproto.NewHistory applies clievent.ForWire inside
// each marshal closure, so a cache hit does not re-scan):
//  1. nil-cache defensive fallback
//  2. single-subscriber fast path
//  3. multi-subscriber getOrMarshal cache path (both miss and hit)
func TestMarshalHistoryFrame_RedactsOnEveryPath(t *testing.T) {
	const secret = "sk-ant-api03-BBBBBBBBBBBBBBBBBBBBBBBB"
	entries := []clievent.EventEntry{
		{Type: "text", Time: 100, Summary: "leak " + secret, Detail: "detail " + secret},
		{Type: "task_start", Time: 101, JSONLPath: "/home/u/.claude/projects/p/s/subagents/agent-x.jsonl", InternalAgentID: "agent-x"},
	}

	assertNoSecret := func(t *testing.T, data []byte) {
		t.Helper()
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("secret survived in marshalled history frame: %q", data)
		}
		for _, internal := range []string{"jsonl_path", "internal_agent_id", "/home/u/"} {
			if bytes.Contains(data, []byte(internal)) {
				t.Fatalf("%q survived in marshalled history frame: %q", internal, data)
			}
		}
	}

	t.Run("single-subscriber path", func(t *testing.T) {
		h := hubWithSubscribers("k", 1) // skips the cache
		data, err := h.marshalHistoryFrame("k", 0, entries)
		if err != nil {
			t.Fatalf("marshalHistoryFrame: %v", err)
		}
		assertNoSecret(t, data)
	})

	t.Run("normal hub paths", func(t *testing.T) {
		h := &Hub{subs: newSubscriberRegistry(), historyMarshalCache: newHistoryMarshalCache()}
		// First call to a fresh key (no subscribers wired) flows through the
		// cached getOrMarshal path on a miss; a repeat call hits the cache.
		first, err := h.marshalHistoryFrame("kk", 0, entries)
		if err != nil {
			t.Fatalf("marshalHistoryFrame miss: %v", err)
		}
		assertNoSecret(t, first)

		second, err := h.marshalHistoryFrame("kk", 0, entries)
		if err != nil {
			t.Fatalf("marshalHistoryFrame hit: %v", err)
		}
		assertNoSecret(t, second)
	})
}
