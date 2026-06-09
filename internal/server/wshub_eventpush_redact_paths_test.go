package server

import (
	"bytes"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestMarshalHistoryFrame_RedactsOnEveryPath pins R20260607-PERF-1 (#1888):
// the redactEntrySecrets call was moved from above the marshal cache down into
// each marshal closure to avoid re-scanning already-redacted entries on cache
// hits. This test guards that the move did NOT open a credential-leak hole —
// the produced "history" frame bytes must never contain the secret on ANY of
// the three serialization paths:
//  1. nil-cache defensive fallback
//  2. single-subscriber fast path
//  3. multi-subscriber getOrMarshal cache path (both miss and hit)
func TestMarshalHistoryFrame_RedactsOnEveryPath(t *testing.T) {
	const secret = "sk-ant-api03-BBBBBBBBBBBBBBBBBBBBBBBB"
	entries := []cli.EventEntry{
		{Type: "text", Time: 100, Summary: "leak " + secret, Detail: "detail " + secret},
	}

	assertNoSecret := func(t *testing.T, data []byte) {
		t.Helper()
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("secret survived in marshalled history frame: %q", data)
		}
	}

	t.Run("nil cache fallback", func(t *testing.T) {
		h := &Hub{} // historyMarshalCache == nil
		data, err := h.marshalHistoryFrame("k", 0, entries)
		if err != nil {
			t.Fatalf("marshalHistoryFrame: %v", err)
		}
		assertNoSecret(t, data)
	})

	t.Run("normal hub paths", func(t *testing.T) {
		h := &Hub{historyMarshalCache: newHistoryMarshalCache()}
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
