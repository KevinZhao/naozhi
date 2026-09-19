package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// TestHubBroadcaster_DropsSysessionErrorMsg pins the one per-subsystem wire
// policy the unified run_ended frame left behind (#2540), and it exists
// because the unification WEAKENED how that policy is enforced. The old
// DaemonRunEnded frame had no ErrorMsg field at all — the type system made
// the leak impossible. The unified RunEnded frame has the field (cron needs
// it), so "sysession daemon errors never reach the browser" (they can echo
// prompt fragments; system-session.md §9.4) is now one if-statement in
// hubBroadcaster. This test is what stands between that if-statement and a
// silent cross-tenant leak.
func TestHubBroadcaster_DropsSysessionErrorMsg(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)
	c := &wsClient{hub: hub, send: make(chan []byte, 8), done: make(chan struct{})}
	c.authenticated.Store(true)
	registerSub(hub, c, "")

	b := newHubBroadcaster(hub)
	ended := func(sub runtelemetry.Subsystem) runtelemetry.RunEndedEvent {
		return runtelemetry.RunEndedEvent{
			Subsystem: sub, OwnerID: "autotitler", RunID: "aaaabbbbccccdddd",
			State: runtelemetry.RunStateFailed, StartedAt: time.Now(), EndedAt: time.Now(),
			DurationMS: 5, Trigger: runtelemetry.TriggerManual,
			ErrorClass: runtelemetry.ErrClassPanic,
			ErrorMsg:   "prompt fragment that must not reach the browser",
		}
	}

	b.BroadcastRunEnded(ended(runtelemetry.SubsystemSysession))
	data, ok := recvRaw(t, c)
	if !ok {
		t.Fatal("no frame delivered")
	}
	var frame wsproto.RunEnded
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if frame.Type != wsproto.TypeRunEnded || frame.Subsystem != "sysession" {
		t.Fatalf("frame = %+v, want a sysession run_ended", frame)
	}
	if frame.ErrorMsg != "" {
		t.Fatalf("sysession error_msg on the wire: %q — daemon errors can echo prompt fragments, "+
			"and every authenticated dashboard client just received one", frame.ErrorMsg)
	}
	if frame.ErrorClass == "" {
		t.Error("error_class dropped along with error_msg; the badge needs the class")
	}

	// The control: cron's ErrorMsg is deliberately on the wire (post-redaction),
	// so a fix that strips it for everyone would break the cron drawer's error
	// display and must fail here.
	b.BroadcastRunEnded(ended(runtelemetry.SubsystemCron))
	data, ok = recvRaw(t, c)
	if !ok {
		t.Fatal("no cron frame delivered")
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("unmarshal cron frame: %v", err)
	}
	if frame.ErrorMsg == "" {
		t.Error("cron error_msg stripped; the policy is per-subsystem, not global")
	}
}
