package cron

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestClassifyOrphanPending is the table for the pure §6.2/§6.5 containment
// state machine extracted in #2172. One row per rule plus rule-priority rows,
// so a future reorder or added branch must be reflected here.
func TestClassifyOrphanPending(t *testing.T) {
	const validID = "run-aabbccddeeff0001-1234567890123456789"
	terminal := &CronRun{State: RunStateFailed, EndedAt: time.Unix(1_700_000_000, 0)}
	open := &CronRun{State: RunStateFailed} // EndedAt zero: not terminal

	cases := []struct {
		name  string
		probe orphanProbe
		want  orphanVerdict
	}{
		{
			name:  "rule1 sandbox unconfigured keeps",
			probe: orphanProbe{sandboxConfigured: false, runtimeSessionID: validID},
			want:  orphanVerdict{orphanKeepPending, orphanReasonSandboxUnconfigured},
		},
		{
			name:  "rule2 malformed session id keeps",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: "not-a-session"},
			want:  orphanVerdict{orphanKeepPending, orphanReasonInvalidSessionID},
		},
		{
			name:  "rule2 empty session id keeps (no handle, §6.2 cannot be satisfied)",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: ""},
			want:  orphanVerdict{orphanKeepPending, orphanReasonInvalidSessionID},
		},
		{
			name:  "rule3 stop failed keeps",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: validID, stopErr: errors.New("api down")},
			want:  orphanVerdict{orphanKeepPending, orphanReasonStopFailed},
		},
		{
			name:  "rule4 already terminal removes without finish (#2054)",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: validID, rec: terminal},
			want:  orphanVerdict{orphanRemoveOnly, orphanReasonAlreadyTerminal},
		},
		{
			name:  "rule5 transient probe error keeps (#2149)",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: validID, recErr: errors.New("read: input/output error")},
			want:  orphanVerdict{orphanKeepPending, orphanReasonProbeTransient},
		},
		{
			name:  "rule6 no record (ErrNotExist) finishes",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: validID, recErr: fmt.Errorf("open: %w", fs.ErrNotExist)},
			want:  orphanVerdict{orphanRemoveAfterFinish, orphanReasonNone},
		},
		{
			name:  "rule6 os.ErrNotExist alias finishes",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: validID, recErr: os.ErrNotExist},
			want:  orphanVerdict{orphanRemoveAfterFinish, orphanReasonNone},
		},
		{
			name:  "rule6 corrupt record finishes (can never be confirmed terminal)",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: validID, recErr: fmt.Errorf("decode: %w", ErrCorruptRun)},
			want:  orphanVerdict{orphanRemoveAfterFinish, orphanReasonNone},
		},
		{
			name:  "rule6 record present but not terminal finishes",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: validID, rec: open},
			want:  orphanVerdict{orphanRemoveAfterFinish, orphanReasonNone},
		},
		{
			name:  "rule6 nil record nil error finishes",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: validID},
			want:  orphanVerdict{orphanRemoveAfterFinish, orphanReasonNone},
		},
		// Priority rows: earlier rules win even when later facts would decide
		// differently — mirrors probeOrphan never gathering the later facts.
		{
			name: "priority unconfigured beats stop-failed and terminal record",
			probe: orphanProbe{sandboxConfigured: false, runtimeSessionID: validID,
				stopErr: errors.New("x"), rec: terminal},
			want: orphanVerdict{orphanKeepPending, orphanReasonSandboxUnconfigured},
		},
		{
			name: "priority invalid id beats stop-failed",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: "bad",
				stopErr: errors.New("x")},
			want: orphanVerdict{orphanKeepPending, orphanReasonInvalidSessionID},
		},
		{
			name: "priority stop-failed beats terminal record",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: validID,
				stopErr: errors.New("x"), rec: terminal},
			want: orphanVerdict{orphanKeepPending, orphanReasonStopFailed},
		},
		{
			name: "priority terminal record beats transient error only when err==nil",
			probe: orphanProbe{sandboxConfigured: true, runtimeSessionID: validID,
				rec: terminal, recErr: errors.New("eio")},
			want: orphanVerdict{orphanKeepPending, orphanReasonProbeTransient},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOrphanPending(tc.probe); got != tc.want {
				t.Fatalf("classifyOrphanPending = {%d,%d}, want {%d,%d}",
					got.decision, got.reason, tc.want.decision, tc.want.reason)
			}
		})
	}
}

// TestOrphanStopBlocked_SharedWithClassifier pins that probeOrphan's I/O gate
// and the classifier's rules 1–2 are the same predicate: if they ever split,
// probeOrphan could Stop a microVM the classifier then classifies as Keep (or
// vice versa) and the two would disagree about whether a Stop was attempted.
func TestOrphanStopBlocked_SharedWithClassifier(t *testing.T) {
	const validID = "run-aabbccddeeff0002-1234567890123456789"
	for _, tc := range []struct {
		configured bool
		id         string
	}{
		{false, validID}, {true, ""}, {true, "nope"}, {true, validID},
	} {
		reason, blocked := orphanStopBlocked(tc.configured, tc.id)
		v := classifyOrphanPending(orphanProbe{sandboxConfigured: tc.configured, runtimeSessionID: tc.id})
		if blocked != (v.decision == orphanKeepPending && v.reason == reason) {
			t.Fatalf("configured=%v id=%q: orphanStopBlocked=(%d,%v) but classifier=%+v",
				tc.configured, tc.id, reason, blocked, v)
		}
	}
}

// TestReconcileOrphan_EmptyRuntimeSessionIDKeepsPending pins the one
// unreachable-in-production path #2172 tightened: a pending record with an
// empty RuntimeSessionID handed straight to reconcileOneSandboxOrphan is KEPT
// (no Stop, no finish, no broadcast) rather than finished-and-removed. The
// upstream scan already drops "" as corrupt (COR-002), so no live deployment
// reaches this branch; encoding Keep here means the classifier never pins a
// §6.2-violating outcome.
func TestReconcileOrphan_EmptyRuntimeSessionIDKeepsPending(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	p := sandboxPending{
		JobID: j.ID, RunID: "abcabcabc0000007",
		RuntimeSessionID: "",
		StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
	}
	path := writePendingFixture(t, storePath, p)

	s.reconcileOneSandboxOrphan(p, path)

	runner.mu.Lock()
	nStopped := len(runner.stopped)
	runner.mu.Unlock()
	if nStopped != 0 {
		t.Fatalf("StopSession called %d time(s) for empty RuntimeSessionID; want 0", nStopped)
	}
	if rec.endedCount() != 0 || rec.startedCount() != 0 {
		t.Fatal("no lifecycle broadcast expected when the orphan is kept")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending file must be kept (fate unconfirmable): %v", err)
	}
}
