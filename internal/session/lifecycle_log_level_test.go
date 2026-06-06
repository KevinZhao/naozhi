package session

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestSessionLifecycleEvents_AreInfo_AuditTrail locks the R214-CODE-5 decision:
// canonical session lifecycle transitions (removed / reset) stay at Info so the
// systemd journal carries an audit trail of every state change. The contrast
// with R84's "creating new session" Debug demotion is intentional — those are
// per-spawn-attempt noise, whereas these fire exactly once per real transition
// and so are bounded by session count, not message traffic.
//
// We drive Remove + Reset against directly-injected, process-less sessions so
// the test needs no live wrapper. The captured Info-level handler MUST observe
// both lines; a future accidental demotion to Debug would silently lose the
// audit row and this test turns it red.
//
// NOT t.Parallel() — slog.SetDefault mutates a process-global. Any parallel
// test that emits a log line would race with this buffer swap.
func TestSessionLifecycleEvents_AreInfo_AuditTrail(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)

	var info bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&info, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	r := newTestRouter(5)

	// "session removed" — Remove walks a present, process-less session.
	rmKey := "feishu:group:t:rm"
	r.ss.sessions[rmKey] = &ManagedSession{key: rmKey}
	if !r.Remove(rmKey) {
		t.Fatalf("Remove(%q) returned false — injected session was not seen", rmKey)
	}

	// "session reset" — Reset takes the same process-less path; the socket
	// wait returns immediately because no shim socket was ever bound.
	rsKey := "feishu:group:t:rs"
	r.ss.sessions[rsKey] = &ManagedSession{key: rsKey}
	r.Reset(rsKey)

	got := info.String()
	for _, want := range []string{"session removed", "session reset"} {
		if !strings.Contains(got, want) {
			t.Errorf("Info-level handler missing %q — lifecycle audit event lost (R214-CODE-5: must stay Info)\nbuffer=%s",
				want, got)
		}
	}
}
