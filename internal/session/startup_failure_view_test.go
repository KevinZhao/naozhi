package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/session/spawnpool"
)

type sfView = sessionview.StartupFailureView

// A dead session's snapshot says what its next send does about the CLI's
// startup failures: wait until retry_at from the second failure in a row,
// start a new conversation when the failure may be the resume's own or the
// backend refused it, else name the class. The key appears in the JSON only
// when there is something to say.
func TestSnapshot_StartupFailure(t *testing.T) {
	t.Parallel()
	at := time.UnixMilli(1_700_000_000_000)
	failed := func(c clierr.ExitClass) processIface { return newStartupFailedProc(c, at) }
	ms := func(d time.Duration) int64 { return at.Add(d).UnixMilli() }
	cases := []struct {
		name     string
		proc     processIface
		before   int32 // startupFails
		rejected bool  // resumeRejected
		want     *sfView
	}{
		{"alive", newIdleProc(), 3, true, nil},
		{"died after startup", newDeadProc(), 2, false, nil},
		{"auth", failed(clierr.ExitAuth), 0, false, &sfView{Class: "auth", Streak: 1}},
		{"mcp config", failed(clierr.ExitMCPConfig), 0, false, &sfView{Class: "mcp_config", Streak: 1}},
		{"missing runtime", failed(clierr.ExitMissingRuntime), 0, false, &sfView{Class: "missing_runtime", Streak: 1}},
		{"unnamed cause", failed(clierr.ExitUnknown), 0, false, &sfView{Class: "unknown", Streak: 1, NewSession: true}},
		{"stale resume id", failed(clierr.ExitResumeNotFound), 0, false, &sfView{Class: "resume_not_found", Streak: 1, NewSession: true}},
		{"second in a row", failed(clierr.ExitAuth), 1, false, &sfView{Class: "auth", Streak: 2, RetryAt: ms(30 * time.Second)}},
		{"third in a row", failed(clierr.ExitUnknown), 2, false, &sfView{Class: "unknown", Streak: 3, RetryAt: ms(time.Minute), NewSession: true}},
		{"cooldown capped", failed(clierr.ExitAuth), 39, false, &sfView{Class: "auth", Streak: 40, RetryAt: ms(startupCooldownMax)}},
		{"rejected resume, auth", failed(clierr.ExitAuth), 0, true, &sfView{Class: "auth", Streak: 1, NewSession: true}},
		{"rejected resume only", newDeadProc(), 0, true, &sfView{NewSession: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &ManagedSession{key: sfKey}
			s.storeProcess(tc.proc)
			s.startupFails.Store(tc.before)
			s.resumeRejected.Store(tc.rejected)
			snap := s.Snapshot()
			if got := snap.StartupFailure; !sameSFView(got, tc.want) {
				t.Errorf("StartupFailure = %+v, want %+v", got, tc.want)
			}
			raw, err := json.Marshal(snap)
			if err != nil {
				t.Fatal(err)
			}
			if has := strings.Contains(string(raw), `"startup_failure":`); has != (tc.want != nil) {
				t.Errorf("JSON has startup_failure = %v, want %v: %s", has, tc.want != nil, raw)
			}
		})
	}
}

// retry_at is exactly where the breaker's cooldown ends.
func TestStartupFailure_RetryAtEndsTheCooldown(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_000_000, 0)
	for streak := int32(0); streak <= 12; streak++ {
		f := startupFailure{streak: streak, at: at}
		end := f.retryAt()
		if end.IsZero() {
			if left := f.cooldownLeft(at); left != 0 {
				t.Errorf("streak %d: no retryAt, but cooldownLeft = %s", streak, left)
			}
			continue
		}
		if left := f.cooldownLeft(at); end.Sub(at) != left || left == 0 {
			t.Errorf("streak %d: retryAt %s after the failure, cooldownLeft %s", streak, end.Sub(at), left)
		}
		if f.cooldownLeft(end) != 0 || f.cooldownLeft(end.Add(-time.Millisecond)) == 0 {
			t.Errorf("streak %d: the cooldown does not end at retryAt", streak)
		}
	}
}

// The session list joins a key's run of failed spawns to its dead session's
// startup_failure when the run failed last, since the breaker pauses on that;
// the session's own Snapshot cannot see the run.
func TestListSessions_StartupFailureJoinsTheKeysRun(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name string
		proc processIface
		run  spawnpool.StartupFailure
		want *sfView
	}{
		{"run after a plain death", newDeadProc(), spawnpool.StartupFailure{Streak: 2, At: now},
			&sfView{Class: "unknown", Streak: 2, RetryAt: now.Add(startupCooldownBase).UnixMilli()}},
		{"run after the process's failure", newStartupFailedProc(clierr.ExitAuth, now.Add(-time.Minute)), spawnpool.StartupFailure{Streak: 3, Class: clierr.ExitMCPConfig, At: now},
			&sfView{Class: "mcp_config", Streak: 3, RetryAt: now.Add(2 * startupCooldownBase).UnixMilli()}},
		{"process failed last", newStartupFailedProc(clierr.ExitAuth, now), spawnpool.StartupFailure{Streak: 1, At: now.Add(-time.Minute)},
			&sfView{Class: "auth", Streak: 1}},
		{"alive", newIdleProc(), spawnpool.StartupFailure{Streak: 2, At: now}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTestRouter(4)
			s := injectSession(r, sfKey, tc.proc)
			injectSession(r, "feishu:direct:bob:general", newDeadProc())
			r.ss.Update(func(tx sessTx) {
				tx.Ext().spawns.NoteStartupFailure(sfKey, tc.run)
				tx.Ext().spawns.NoteStartupFailure("feishu:direct:nobody:general", tc.run)
			})
			var got *sfView
			for _, snap := range r.ListSessions() {
				switch snap.Key {
				case sfKey:
					got = snap.StartupFailure
				default:
					if snap.StartupFailure != nil {
						t.Errorf("%s: StartupFailure = %+v, want nil", snap.Key, snap.StartupFailure)
					}
				}
			}
			if !sameSFView(got, tc.want) {
				t.Errorf("listed StartupFailure = %+v, want %+v", got, tc.want)
			}
			if own, want := s.Snapshot().StartupFailure, startupFailureView(s, spawnpool.StartupFailure{}, false); !sameSFView(own, want) {
				t.Errorf("Snapshot StartupFailure = %+v, want the process's own %+v", own, want)
			}
		})
	}
}

func sameSFView(a, b *sfView) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
