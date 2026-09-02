package selfupdate

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestStatusSnapshotAction is the guard on the single most consequential piece
// of logic in this feature: deciding whether a pending update needs to be
// DOWNLOADED or merely APPLIED by restarting.
//
// Getting it wrong in the staged direction is not a cosmetic bug. Replace()
// backs up whatever currently sits at the install path, so offering "install"
// while a new binary is already staged makes a second Replace copy the NEW
// binary over the backup — destroying the only artifact that could roll the
// deployment back. See docs/rfc/dashboard-update-notice.md §1.3.
func TestStatusSnapshotAction(t *testing.T) {
	tests := []struct {
		name string
		snap StatusSnapshot
		want Action
	}{
		{
			name: "no check yet",
			snap: StatusSnapshot{Current: "v1.0.0"},
			want: ActionNone,
		},
		{
			name: "already latest",
			snap: StatusSnapshot{Current: "v1.0.0", Latest: "v1.0.0"},
			want: ActionNone,
		},
		{
			name: "newer release, nothing staged",
			snap: StatusSnapshot{Current: "v1.0.0", Latest: "v1.1.0"},
			want: ActionInstall,
		},
		{
			// The steady state under the default "download" mode, and the case
			// that must NOT report ActionInstall even though Latest > Current.
			name: "staged binary awaiting restart",
			snap: StatusSnapshot{Current: "v1.0.0", Latest: "v1.1.0", Staged: "v1.1.0"},
			want: ActionRestart,
		},
		{
			// Staged tag equal to the running version means the restart already
			// happened; there is nothing left to do.
			name: "staged tag already running",
			snap: StatusSnapshot{Current: "v1.1.0", Latest: "v1.1.0", Staged: "v1.1.0"},
			want: ActionNone,
		},
		{
			// Downgrade protection (R20260602141221-SEC-1): a remote tag OLDER
			// than what we run must never be offered as an update.
			name: "remote older than current",
			snap: StatusSnapshot{Current: "v1.2.0", Latest: "v1.0.0"},
			want: ActionNone,
		},
		{
			// semverGreater returns false on unparseable input, which has to
			// land on ActionNone rather than defaulting to "offer an upgrade".
			name: "unparseable remote tag",
			snap: StatusSnapshot{Current: "v1.0.0", Latest: "not-a-version"},
			want: ActionNone,
		},
		{
			name: "dev build never offered an install",
			snap: StatusSnapshot{Current: "dev", Latest: "v1.1.0"},
			want: ActionNone,
		},
		{
			// A staged binary is actionable even from a dev build: the bytes on
			// disk really are newer than the running process, and restarting
			// really does apply them. (Whether the operator SHOULD is a
			// preflight concern, not an Action one.)
			name: "staged from dev build",
			snap: StatusSnapshot{Current: "dev", Latest: "v1.1.0", Staged: "v1.1.0"},
			want: ActionRestart,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.snap.Action(); got != tc.want {
				t.Fatalf("Action() = %q, want %q (snap=%+v)", got, tc.want, tc.snap)
			}
		})
	}
}

// TestStatusNilReceiverIsNoOp locks in that every method tolerates a nil
// *Status. This is what lets the Checker run with reporting disabled — and it
// is why the pre-existing checker tests still exercise the original code path
// instead of having been rewritten around the new field.
func TestStatusNilReceiverIsNoOp(t *testing.T) {
	var s *Status
	// None of these may panic.
	s.noteCheck("v1.0.0", nil)
	s.notePhase(PhaseStaged, "v1.0.0", errors.New("boom"))
	if got := s.Snapshot(); got != (StatusSnapshot{}) {
		t.Fatalf("nil Snapshot() = %+v, want zero value", got)
	}
	if got := s.Current(); got != "" {
		t.Fatalf("nil Current() = %q, want empty", got)
	}
	at, latest := s.LastCheck()
	if !at.IsZero() || latest != "" {
		t.Fatalf("nil LastCheck() = (%v, %q), want (zero, \"\")", at, latest)
	}
}

func TestStatusNoteCheck(t *testing.T) {
	t.Run("success advances phase to available", func(t *testing.T) {
		s := NewStatus("v1.0.0")
		s.noteCheck("v1.1.0", nil)
		got := s.Snapshot()
		if got.Latest != "v1.1.0" {
			t.Fatalf("Latest = %q, want v1.1.0", got.Latest)
		}
		if got.Phase != PhaseAvailable {
			t.Fatalf("Phase = %q, want %q", got.Phase, PhaseAvailable)
		}
		if got.CheckedAt.IsZero() {
			t.Fatal("CheckedAt not set after successful check")
		}
	})

	t.Run("not-newer keeps phase idle", func(t *testing.T) {
		s := NewStatus("v1.1.0")
		s.noteCheck("v1.1.0", nil)
		if got := s.Snapshot(); got.Phase != PhaseIdle {
			t.Fatalf("Phase = %q, want %q", got.Phase, PhaseIdle)
		}
	})

	t.Run("failure preserves a previously known latest", func(t *testing.T) {
		// A transient network blip must not make a real pending update
		// disappear from the dashboard.
		s := NewStatus("v1.0.0")
		s.noteCheck("v1.1.0", nil)
		s.noteCheck("", errors.New("dial tcp: lookup github.com: no such host"))
		got := s.Snapshot()
		if got.Latest != "v1.1.0" {
			t.Fatalf("Latest = %q after failed check, want it preserved as v1.1.0", got.Latest)
		}
		if got.CheckErr == "" {
			t.Fatal("CheckErr empty after failed check")
		}
		if got.CheckedAt.IsZero() {
			t.Fatal("CheckedAt must advance even on failure (the on-demand throttle depends on it)")
		}
	})

	t.Run("does not clobber a staged phase", func(t *testing.T) {
		// A periodic check landing after an install must not erase the fact
		// that a binary is staged — that state is the whole signal.
		s := NewStatus("v1.0.0")
		s.notePhase(PhaseStaged, "v1.1.0", nil)
		s.noteCheck("v1.1.0", nil)
		got := s.Snapshot()
		if got.Phase != PhaseStaged {
			t.Fatalf("Phase = %q after check, want %q preserved", got.Phase, PhaseStaged)
		}
		if got.Staged != "v1.1.0" {
			t.Fatalf("Staged = %q, want v1.1.0 preserved", got.Staged)
		}
	})
}

func TestStatusNotePhase(t *testing.T) {
	t.Run("empty staged does not clear a real staged tag", func(t *testing.T) {
		s := NewStatus("v1.0.0")
		s.notePhase(PhaseStaged, "v1.1.0", nil)
		s.notePhase(PhaseFailed, "", errors.New("restart trigger failed"))
		got := s.Snapshot()
		if got.Staged != "v1.1.0" {
			t.Fatalf("Staged = %q, want v1.1.0 (a later transition without a tag must not clear it)", got.Staged)
		}
		if got.LastErr == "" {
			t.Fatal("LastErr empty after failing transition")
		}
	})

	t.Run("success clears a stale error", func(t *testing.T) {
		s := NewStatus("v1.0.0")
		s.notePhase(PhaseFailed, "", errors.New("download failed"))
		s.notePhase(PhaseStaged, "v1.1.0", nil)
		if got := s.Snapshot(); got.LastErr != "" {
			t.Fatalf("LastErr = %q, want cleared after a successful transition", got.LastErr)
		}
	})
}

// TestStatusConcurrentAccess is a -race gate: the Checker writes from its
// background goroutine while the HTTP handler reads on request goroutines.
func TestStatusConcurrentAccess(t *testing.T) {
	s := NewStatus("v1.0.0")
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.noteCheck("v1.1.0", nil)
				s.notePhase(PhaseInstalling, "", nil)
				s.notePhase(PhaseStaged, "v1.1.0", nil)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.Snapshot().Action()
				_ = s.Current()
				_, _ = s.LastCheck()
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
