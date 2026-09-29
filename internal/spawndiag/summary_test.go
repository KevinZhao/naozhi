package spawndiag

import (
	"fmt"
	"testing"
)

// The summary counts first occurrences per layer|action, keeps the latest
// entries up to its cap, and carries neither reason nor scope.
func TestSummary(t *testing.T) {
	resetSummaryForTest()
	t.Cleanup(resetSummaryForTest)
	if _, ok := Snapshot(); ok {
		t.Fatal("an empty summary reported itself")
	}
	One("dashboard:direct:chat-secret:general", "env-filter", "AWS_ENDPOINT_URL", "dropped", "value fails its guard: sentinel-value")
	One("dashboard:direct:chat-secret:general", "env-filter", "AWS_ENDPOINT_URL", "dropped", "repeat")
	for i := 0; i < recentCap+5; i++ {
		One("config", "config-invalid", fmt.Sprintf("k%d", i), "fallback", "r")
	}
	s, ok := Snapshot()
	if !ok {
		t.Fatal("no summary")
	}
	if s.Counts["env-filter|dropped"] != 1 {
		t.Errorf("env-filter|dropped = %d, want 1 (a repeat in the same scope is not a new occurrence)", s.Counts["env-filter|dropped"])
	}
	if s.Counts["config-invalid|fallback"] != int64(recentCap+5) {
		t.Errorf("config-invalid|fallback = %d", s.Counts["config-invalid|fallback"])
	}
	if len(s.Recent) != recentCap || s.Recent[len(s.Recent)-1].Key != fmt.Sprintf("k%d", recentCap+4) {
		t.Errorf("recent = %d entries ending %+v, want the latest %d", len(s.Recent), s.Recent[len(s.Recent)-1], recentCap)
	}
	s.Counts["x"] = 1
	s.Recent[0].Key = "mutated"
	if again, _ := Snapshot(); again.Counts["x"] != 0 || again.Recent[0].Key == "mutated" {
		t.Error("Snapshot returned the live state, not a copy")
	}
}
