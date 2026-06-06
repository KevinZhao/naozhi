package session

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

// TestLogSessionLifecycle_StructuredInfo pins the R214-CODE-5 (#422) decision:
// session lifecycle events are emitted at Info with a uniform `event` + `key`
// attribute pair so they can be filtered/aggregated as one audit stream.
func TestLogSessionLifecycle_StructuredInfo(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logSessionLifecycle("expired", "dashboard:direct:user:general", "idle", "5m")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("log line not valid JSON: %v (%q)", err, buf.String())
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if rec["msg"] != "session expired" {
		t.Errorf("msg = %v, want %q", rec["msg"], "session expired")
	}
	if rec["event"] != "expired" {
		t.Errorf("event = %v, want expired", rec["event"])
	}
	if rec["key"] != "dashboard:direct:user:general" {
		t.Errorf("key = %v", rec["key"])
	}
	if rec["idle"] != "5m" {
		t.Errorf("idle = %v, want 5m", rec["idle"])
	}
}

// TestSessionLifecycleLevel_Info documents that the centralised level stays at
// Info; a deliberate change to Debug must update this pin.
func TestSessionLifecycleLevel_Info(t *testing.T) {
	if sessionLifecycleLevel != slog.LevelInfo {
		t.Fatalf("sessionLifecycleLevel = %v, want Info (audit-first decision, #422)", sessionLifecycleLevel)
	}
}
