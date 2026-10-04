package cron

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// watermarkSession is gatedSendSession plus the SendWatermarker capability.
// The watermark it reports changes once Send is entered, so a stamp taken
// after the turn started would show up as the wrong value.
type watermarkSession struct {
	*gatedSendSession
	mark string
}

func (w *watermarkSession) SendWatermark() string {
	select {
	case <-w.entered:
		return "after-send"
	default:
		return w.mark
	}
}

type watermarkRouter struct {
	reapRouter
	sess Session
}

func (r *watermarkRouter) GetOrCreate(_ context.Context, _ string, _ AgentOpts) (Session, SessionStatus, error) {
	return r.sess, SessionNew, nil
}

// runAndReadMarker starts one local run on sess, and returns its marker as
// read while Send is blocked — the only moment the marker exists.
func runAndReadMarker(t *testing.T, sess Session, gate *gatedSendSession) (runInflightMarker, *Job) {
	t.Helper()
	s, _ := newSchedulerWithStore(t)
	s.router = &watermarkRouter{sess: sess}
	j := &Job{ID: mustGenerateID(), Schedule: "@every 5m", Prompt: "do thing", WorkDir: "/tmp/wd"}
	s.putJobForTest(j)

	done := make(chan struct{})
	go func() { s.executeOpt(j.ID, true /* viaTriggerNow: skip jitter */); close(done) }()
	t.Cleanup(func() {
		close(gate.release)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("run did not finish")
		}
	})
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Send was never entered")
	}
	names := markerFiles(t, s)
	if len(names) != 1 {
		t.Fatalf("markers while in flight = %v, want exactly 1", names)
	}
	m, ok := s.readRunInflightMarker(filepath.Join(s.runInflightDir(), names[0]))
	if !ok {
		t.Fatal("marker unreadable while in flight")
	}
	return m, j
}

// TestExecSend_StampsTheWatermarkBeforeSend: the watermark is what lets the
// next boot adopt a result already in the replayed backlog, so it has to be on
// disk by the time the turn starts, taken before it, and must not cost the
// marker any of the fields the interrupted record needs.
func TestExecSend_StampsTheWatermarkBeforeSend(t *testing.T) {
	t.Parallel()
	gate := &gatedSendSession{entered: make(chan struct{}), release: make(chan struct{})}
	m, j := runAndReadMarker(t, &watermarkSession{gatedSendSession: gate, mark: "4242:9"}, gate)

	if m.SendWatermark != "4242:9" {
		t.Errorf("SendWatermark = %q, want %q (taken just before Send)", m.SendWatermark, "4242:9")
	}
	if m.JobID != j.ID || m.Prompt != "do thing" || m.WorkDir != "/tmp/wd" || m.StartedAtMS <= 0 || m.RunID == "" {
		t.Errorf("rewritten marker lost its identity: %+v", m)
	}
}

// TestExecSend_NoWatermarkCapabilityLeavesTheMarkerAlone: a session without the
// capability gets the marker as executeAcquired wrote it.
func TestExecSend_NoWatermarkCapabilityLeavesTheMarkerAlone(t *testing.T) {
	t.Parallel()
	gate := &gatedSendSession{entered: make(chan struct{}), release: make(chan struct{})}
	m, j := runAndReadMarker(t, gate, gate)

	if m.SendWatermark != "" {
		t.Errorf("SendWatermark = %q for a session without the capability, want empty", m.SendWatermark)
	}
	if m.JobID != j.ID {
		t.Errorf("JobID = %q, want %q", m.JobID, j.ID)
	}
}

// TestRunInflightMarker_WatermarkIsAdditive: a marker from an older binary has
// no adopt_after and still parses, as one with no watermark.
func TestRunInflightMarker_WatermarkIsAdditive(t *testing.T) {
	t.Parallel()
	s, _ := newSchedulerWithStore(t)
	dir := s.runInflightDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old.json")
	if err := os.WriteFile(old, []byte(`{"job_id":"j1","run_id":"r1","started_at_ms":1700000000000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, ok := s.readRunInflightMarker(old)
	if !ok || m.SendWatermark != "" {
		t.Errorf("old marker = (%+v, %v), want parsed with no watermark", m, ok)
	}

	b, err := json.Marshal(runInflightMarker{JobID: "j1", RunID: "r2", StartedAtMS: 1, SendWatermark: "4242:9"})
	if err != nil {
		t.Fatal(err)
	}
	var back runInflightMarker
	if err := json.Unmarshal(b, &back); err != nil || back.SendWatermark != "4242:9" {
		t.Errorf("round trip = (%+v, %v) from %s", back, err, b)
	}
}

// TestReconcile_HandsTheMarkersWatermarkToTheAdopter: the adopter decides
// whether a replayed result is this run's by the watermark, so the reconcile
// must pass the one the run recorded — and "" for a marker that has none.
func TestReconcile_HandsTheMarkersWatermarkToTheAdopter(t *testing.T) {
	t.Parallel()
	router := &adoptingRouter{verdicts: map[string]AdoptVerdict{}, runs: map[string]*fakeInFlightRun{}}
	s, jobID, _, _ := seedMarkedRun(t, router, 0)
	other := mustGenerateID()
	s.putJobForTest(&Job{ID: other, Schedule: "@every 5m", Prompt: "other"})
	if s.writeRunInflightMarker(runInflightMarker{
		JobID: other, RunID: mustGenerateRunID(), StartedAtMS: time.Now().UnixMilli(), SendWatermark: "4242:9",
	}, slog.Default()) == "" {
		t.Fatal("marker write failed")
	}

	s.reconcileRunInflight()
	s.gcWG.Wait()

	router.afterMu.Lock()
	defer router.afterMu.Unlock()
	if got, ok := router.after["cron:"+other]; !ok || got != "4242:9" {
		t.Errorf("watermark passed for the stamped run = (%q, %v), want (%q, true)", got, ok, "4242:9")
	}
	if got, ok := router.after["cron:"+jobID]; !ok || got != "" {
		t.Errorf("watermark passed for the unstamped run = (%q, %v), want (\"\", true)", got, ok)
	}
}
