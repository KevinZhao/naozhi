package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

const (
	scopeOther = "22222222-3333-4444-5555-666666666666" // ledger entries, no transcript
	scopeFork  = "33333333-4444-5555-6666-777777777777"
	scopeKey2  = "dashboard:direct:other:general"
	scopeKey3  = "dashboard:direct:fork:general"
)

// reconcileScope is a bare ledger and claude dir: rcSID under rcKey,
// scopeOther under scopeKey2 and scopeFork under scopeKey3, priced at $1 per
// 1000 cache-read tokens.
type reconcileScope struct {
	opts  reconcileOpts
	today time.Time
}

func newReconcileScope(t *testing.T) reconcileScope {
	t.Helper()
	dir := t.TempDir()
	s := reconcileScope{today: time.Now().UTC().Truncate(24 * time.Hour)}
	s.opts = reconcileOpts{SessionStorePath: filepath.Join(dir, "sessions.json"), ClaudeDir: filepath.Join(dir, "claude"),
		CronStorePath: filepath.Join(dir, "cron", "cron_jobs.json"), Until: s.today}
	writeJSON(t, s.opts.SessionStorePath, []map[string]any{
		{"key": rcKey, "session_id": rcSID}, {"key": scopeKey2, "session_id": scopeOther}, {"key": scopeKey3, "session_id": scopeFork},
	})
	return s
}

// day is n days from today at hh:mm UTC.
func (s reconcileScope) day(n, hh, mm int) time.Time {
	return s.today.Add(time.Duration(n)*24*time.Hour + time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
}

// runStarted writes rcSID's session-runs record for runID.
func (s reconcileScope) runStarted(t *testing.T, runID string, at time.Time) {
	t.Helper()
	s.sessionRun(t, runhistory.SessionRun{RunID: runID, SessionKey: rcKey, SessionID: rcSID, StartedAt: at})
}

func (s reconcileScope) sessionRun(t *testing.T, r runhistory.SessionRun) {
	t.Helper()
	writeJSON(t, filepath.Join(datadir.ForStore(s.opts.SessionStorePath).SessionRunsRoot(), "abcd", r.RunID+".json"), r)
}

func (s reconcileScope) transcript(t *testing.T, sid string, lines ...string) {
	t.Helper()
	writeRaw(t, claudefs.TranscriptIn(filepath.Join(claudefs.ProjectsRoot(s.opts.ClaudeDir), "-w-naozhi"), sid), lines...)
}

// scopeMsg is an assistant line of usd at $1 per 1000 cache-read tokens;
// entrypoint "" leaves the field out.
func scopeMsg(ts time.Time, id string, usd float64, entrypoint string) string {
	v := map[string]any{"type": "assistant", "timestamp": ts.Format(time.RFC3339Nano), "sessionId": rcSID,
		"message": map[string]any{"id": id, "model": rcModel, "role": "assistant",
			"usage": map[string]any{"cache_read_input_tokens": int64(usd * 1000)}}}
	if entrypoint != "" {
		v["entrypoint"] = entrypoint
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (s reconcileScope) run(t *testing.T) (reconcileReport, string) {
	t.Helper()
	var out bytes.Buffer
	rep, err := reconcileLedger(s.opts, &out)
	if err != nil {
		t.Fatalf("reconcile: %v\n%s", err, out.String())
	}
	return rep, out.String()
}

func settlementOf(rep reconcileReport, sid string) sessionSettlement {
	for _, st := range rep.Sessions {
		if st.SessionID == sid {
			return st
		}
	}
	return sessionSettlement{}
}

// A turn whose lines start before -until and whose result is booked after it
// leaves the day before alone: that day's share is in the later entry, so a
// residual would book it twice. So does a turn still running, its result not
// booked yet, and a turn across an earlier midnight leaves both its days
// alone, the second of which books the first's share.
func TestReconcile_TurnAcrossUntilIsNotBookedTwice(t *testing.T) {
	for _, c := range []struct {
		name  string
		lines func(s reconcileScope) []string
		turns func(s reconcileScope) []costledger.Entry
		open  int
	}{
		{"booked after until",
			func(s reconcileScope) []string {
				return []string{scopeMsg(s.day(-1, 23, 30), "msg_1", 5, "sdk-cli"), scopeMsg(s.day(0, 0, 30), "msg_2", 5, "sdk-cli")}
			},
			func(s reconcileScope) []costledger.Entry {
				return []costledger.Entry{rcTurn(s.day(0, 0, 31), rcKey, "aaaaaaaaaaaaaaaa", 10)}
			}, 1},
		{"booked the next settled day",
			func(s reconcileScope) []string {
				return []string{scopeMsg(s.day(-2, 23, 30), "msg_1", 5, "sdk-cli"), scopeMsg(s.day(-1, 0, 30), "msg_2", 5, "sdk-cli")}
			},
			func(s reconcileScope) []costledger.Entry {
				return []costledger.Entry{rcTurn(s.day(-1, 0, 31), rcKey, "aaaaaaaaaaaaaaaa", 10)}
			}, 2},
		{"not booked yet",
			func(s reconcileScope) []string {
				return []string{scopeMsg(s.day(-1, 10, 0), "msg_1", 2, "sdk-cli"), scopeMsg(s.day(-1, 23, 30), "msg_2", 5, "sdk-cli")}
			},
			func(s reconcileScope) []costledger.Entry {
				return []costledger.Entry{rcTurn(s.day(-1, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 2)}
			}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newReconcileScope(t)
			s.transcript(t, rcSID, c.lines(s)...)
			s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-3, 0, 0))
			seedLedger(t, s.opts.SessionStorePath, append(c.turns(s), rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))...)
			rep, out := s.run(t)
			if len(rep.Planned) != 0 {
				t.Fatalf("planned %+v, want nothing\n%s", rep.Planned, out)
			}
			if st := settlementOf(rep, rcSID); st.OpenDays != c.open {
				t.Errorf("open days = %d, want %d reported\n%s", st.OpenDays, c.open, out)
			}
		})
	}
}

// A session naozhi took over from a terminal, or resumed from history, keeps
// its CLI session id, so its transcript holds the earlier turns too.
// Interactive lines are not naozhi's spend and are left out; lines of no
// known origin, or from before the run of the session's first entry, hold
// their day rather than book it.
func TestReconcile_TakenOverHistoryIsNotBooked(t *testing.T) {
	for _, c := range []struct {
		entrypoint string
		transcript float64
	}{{"cli", 2}, {"claude-vscode", 2}, {"", 72}, {"sdk-ts", 72}, {"sdk-cli", 72}} {
		t.Run("entrypoint="+c.entrypoint, func(t *testing.T) {
			s := newReconcileScope(t)
			s.transcript(t, rcSID, scopeMsg(s.day(-2, 10, 0), "msg_old", 40, c.entrypoint),
				scopeMsg(s.day(-1, 9, 0), "msg_today", 30, c.entrypoint), scopeMsg(s.day(-1, 10, 0), "msg_nz", 2, "sdk-cli"))
			s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-1, 9, 59))
			seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-1, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 2),
				rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
			rep, out := s.run(t)
			if len(rep.Planned) != 0 {
				t.Fatalf("planned %+v, want nothing\n%s", rep.Planned, out)
			}
			st := settlementOf(rep, rcSID)
			if !near(st.Transcript, c.transcript) || !near(st.After, 2) {
				t.Errorf("settlement = %+v, want transcript %v and the ledger left at 2\n%s", st, c.transcript, out)
			}
		})
	}
}

// Headless lines from before the run of the session's first entry, the same
// day, are another process's: the session was resumed from history.
func TestReconcile_LinesBeforeTheFirstRunHoldTheirDay(t *testing.T) {
	s := newReconcileScope(t)
	s.transcript(t, rcSID, scopeMsg(s.day(-1, 9, 0), "msg_before", 30, "sdk-cli"), scopeMsg(s.day(-1, 10, 0), "msg_nz", 2, "sdk-cli"))
	s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-1, 9, 59))
	seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-1, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 2),
		rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
	rep, out := s.run(t)
	if len(rep.Planned) != 0 {
		t.Fatalf("planned %+v, want nothing\n%s", rep.Planned, out)
	}
	if st := settlementOf(rep, rcSID); st.OpenDays != 1 {
		t.Errorf("open days = %d, want 1\n%s", st.OpenDays, out)
	}
}

// A line of no known origin inside a naozhi run still holds its day: an old
// CLI wrote no entrypoint, and another client could share the session.
func TestReconcile_LinesOfUnknownOriginHoldTheirDay(t *testing.T) {
	s := newReconcileScope(t)
	s.transcript(t, rcSID, scopeMsg(s.day(-1, 10, 0), "msg_1", 5, ""))
	s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-1, 9, 59))
	seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-1, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 2),
		rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
	rep, out := s.run(t)
	if len(rep.Planned) != 0 {
		t.Fatalf("planned %+v, want nothing\n%s", rep.Planned, out)
	}
	if st := settlementOf(rep, rcSID); st.ForeignDays != 1 {
		t.Errorf("foreign days = %d, want 1\n%s", st.ForeignDays, out)
	}
}

// The residual still books what a naozhi run spent and no entry carried.
func TestReconcile_BooksAnUnbookedShareOfANaozhiRun(t *testing.T) {
	s := newReconcileScope(t)
	s.transcript(t, rcSID, scopeMsg(s.day(-1, 10, 0), "msg_1", 5, "sdk-cli"))
	s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-1, 9, 59))
	seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-1, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 2),
		rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
	rep, out := s.run(t)
	if len(rep.Planned) != 1 || !near(rep.Planned[0].Amount, 3) {
		t.Fatalf("planned %+v, want one +3\n%s", rep.Planned, out)
	}
}

// A dashboard key that resumes a cron run's session inherits its turns in the
// transcript; cron booked them under its own source, so a day a cron run of
// the session touched gets no residual.
func TestReconcile_CronTurnsOfAResumedSessionAreNotBooked(t *testing.T) {
	s := newReconcileScope(t)
	s.transcript(t, rcSID, scopeMsg(s.day(-1, 9, 10), "msg_cron", 5, "sdk-cli"), scopeMsg(s.day(-1, 11, 0), "msg_nz", 3, "sdk-cli"))
	s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-1, 9, 0))
	writeJSON(t, filepath.Join(datadir.ForStore(s.opts.CronStorePath).RunsRoot(), "0123456789abcdef", "c.json"),
		cron.CronRun{RunID: "cccccccccccccccc", JobID: "0123456789abcdef", SessionID: rcSID, StartedAt: s.day(-1, 9, 0), EndedAt: s.day(-1, 9, 30), CostUSD: 5, Fresh: true})
	cronTurn := rcTurn(s.day(-1, 9, 30), "", "cccccccccccccccc", 5)
	cronTurn.Source, cronTurn.JobID = costledger.SourceCronLocal, "0123456789abcdef"
	seedLedger(t, s.opts.SessionStorePath, cronTurn, rcTurn(s.day(-1, 11, 1), rcKey, "aaaaaaaaaaaaaaaa", 3),
		rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
	rep, out := s.run(t)
	if len(rep.Planned) != 0 {
		t.Fatalf("planned %+v, want nothing\n%s", rep.Planned, out)
	}
	if st := settlementOf(rep, rcSID); st.ForeignDays != 1 {
		t.Errorf("foreign days = %d, want the cron run's day reported\n%s", st.ForeignDays, out)
	}
}

// -session settles a fork as a full run would: its parent still claims the
// lines the fork copied.
func TestReconcile_SessionFilterKeepsAForksCopiedLinesWithItsParent(t *testing.T) {
	s := newReconcileScope(t)
	parent := scopeMsg(s.day(-1, 10, 0), "msg_parent", 5, "sdk-cli")
	s.transcript(t, rcSID, parent)
	s.transcript(t, scopeFork, parent, scopeMsg(s.day(-1, 11, 0), "msg_fork", 3, "sdk-cli"))
	s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-1, 9, 0))
	s.sessionRun(t, runhistory.SessionRun{RunID: "cccccccccccccccc", SessionKey: scopeKey3, SessionID: scopeFork, StartedAt: s.day(-1, 9, 0)})
	seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-1, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 5),
		rcTurn(s.day(-1, 11, 1), scopeKey3, "cccccccccccccccc", 3))
	for _, only := range []string{"", scopeFork} {
		s.opts.Session = only
		rep, out := s.run(t)
		if len(rep.Planned) != 0 {
			t.Fatalf("-session %q planned %+v, want nothing\n%s", only, rep.Planned, out)
		}
		if st := settlementOf(rep, scopeFork); !near(st.Transcript, 3) {
			t.Errorf("-session %q: fork transcript = %v, want its own 3\n%s", only, st.Transcript, out)
		}
	}
}
