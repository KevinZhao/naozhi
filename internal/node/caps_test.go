package node

import (
	"strings"
	"testing"
)

// TestLogUnknownCaps covers three cases: empty (no-op), all-known (no WARN),
// and mixed (WARN lists only unknown tags). Uses the captureSlog helper
// defined in reverseserver_host_log_test.go. R212-ARCH-402.
func TestLogUnknownCaps(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		buf, mu, restore := captureSlog(t)
		defer restore()
		logUnknownCaps("n", nil)
		logUnknownCaps("n", []string{})
		mu.Lock()
		defer mu.Unlock()
		if buf.String() != "" {
			t.Errorf("expected no log, got: %q", buf.String())
		}
	})
	t.Run("AllKnown", func(t *testing.T) {
		buf, mu, restore := captureSlog(t)
		defer restore()
		all := make([]string, 0, len(knownServerCaps))
		for c := range knownServerCaps {
			all = append(all, c)
		}
		logUnknownCaps("n", all)
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(buf.String(), "unknown capabilities") {
			t.Errorf("expected no WARN, got: %q", buf.String())
		}
	})
	t.Run("SomeUnknown", func(t *testing.T) {
		buf, mu, restore := captureSlog(t)
		defer restore()
		logUnknownCaps("node-42", []string{"gemini", "futurecap", "acp", "another"})
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		if !strings.Contains(got, "reverse node advertised unknown capabilities") {
			t.Fatalf("expected WARN, got: %q", got)
		}
		if !strings.Contains(got, "node_id=node-42") ||
			!strings.Contains(got, "futurecap") || !strings.Contains(got, "another") {
			t.Errorf("missing expected attrs, got: %q", got)
		}
		// Known caps must be filtered from unknown_caps slice.
		if i := strings.Index(got, "unknown_caps=["); i >= 0 {
			if j := strings.Index(got[i:], "]"); j > 0 {
				sec := got[i : i+j]
				if strings.Contains(sec, "gemini") || strings.Contains(sec, "acp") {
					t.Errorf("known caps leaked: %q", sec)
				}
			}
		}
	})
}
