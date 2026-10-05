package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
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
// day, are another process's: the session was resumed from history. A run
// record without a start leaves the run starting at the entry.
func TestReconcile_LinesBeforeTheFirstRunHoldTheirDay(t *testing.T) {
	for _, started := range []bool{true, false} {
		s := newReconcileScope(t)
		s.transcript(t, rcSID, scopeMsg(s.day(-1, 9, 0), "msg_before", 30, "sdk-cli"), scopeMsg(s.day(-1, 10, 0), "msg_nz", 2, "sdk-cli"))
		run := runhistory.SessionRun{RunID: "aaaaaaaaaaaaaaaa", SessionKey: rcKey, SessionID: rcSID, EndedAt: s.day(-1, 10, 1)}
		if started {
			run.StartedAt = s.day(-1, 9, 59)
		}
		s.sessionRun(t, run)
		seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-1, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 2),
			rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
		rep, out := s.run(t)
		if len(rep.Planned) != 0 {
			t.Fatalf("started=%v: planned %+v, want nothing\n%s", started, rep.Planned, out)
		}
		if st := settlementOf(rep, rcSID); st.OpenDays != 1 {
			t.Errorf("started=%v: open days = %d, want 1\n%s", started, st.OpenDays, out)
		}
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

// A turn correctly baselined after a small restore is genuine, however much
// larger than the restore it is, and so is every later turn of its process.
func TestReconcile_ABaselinedTurnAfterASmallRestoreIsNotFlagged(t *testing.T) {
	s := newReconcileScope(t)
	s.transcript(t, rcSID, scopeMsg(s.day(-2, 10, 0), "msg_1", 0.6, "sdk-cli"), rcCostState(0.6, 600),
		rcLine("queue-operation", s.day(-1, 9, 0), "", 0),
		scopeMsg(s.day(-1, 9, 1), "msg_2", 3, "sdk-cli"), scopeMsg(s.day(-1, 10, 1), "msg_3", 1, "sdk-cli"))
	s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-2, 9, 59))
	seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-2, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 0.6),
		rcTurn(s.day(-1, 9, 2), rcKey, "cccccccccccccccc", 3), rcTurn(s.day(-1, 10, 2), rcKey, "dddddddddddddddd", 1),
		rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
	rep, out := s.run(t)
	if len(rep.Flagged) != 0 || len(rep.Planned) != 0 {
		t.Fatalf("flagged %+v planned %+v, want nothing\n%s", rep.Flagged, rep.Planned, out)
	}
	if st := settlementOf(rep, rcSID); !near(st.After, 4.6) || !near(st.Transcript, 4.6) {
		t.Errorf("settlement = %+v, want after = transcript = 4.60\n%s", st, out)
	}
}

// The turn of an entry starts no earlier than the cost-state: a message the
// writing process ran after its last booked entry is part of the restored
// total, so it does not hide that the next process charged that total again.
func TestReconcile_UnbookedSpendBeforeTheCostStateIsNotTheTurn(t *testing.T) {
	s := newReconcileScope(t)
	s.transcript(t, rcSID, scopeMsg(s.day(-2, 10, 0), "msg_1", 2, "sdk-cli"), scopeMsg(s.day(-2, 11, 0), "msg_k", 10, "sdk-cli"),
		rcCostState(12, 12000), rcLine("queue-operation", s.day(-1, 9, 0), "", 0), scopeMsg(s.day(-1, 9, 1), "msg_2", 3, "sdk-cli"))
	s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-2, 9, 59))
	seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-2, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 2),
		rcTurn(s.day(-1, 9, 2), rcKey, "cccccccccccccccc", 15), rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
	rep, out := s.run(t)
	if len(rep.Flagged) != 1 || rep.Flagged[0].Entry.RunID != "cccccccccccccccc" || !near(rep.Planned[0].Amount, -12) {
		t.Fatalf("flagged %+v planned %+v, want the resumed turn flagged -12\n%s", rep.Flagged, rep.Planned, out)
	}
}

// Each process that resumes the same cost-state, the one before it killed
// and so writing none, is judged on its own turn: the earlier one's turn,
// booked by its result or by its process-end partial, is not in it.
func TestReconcile_EachResumeOfOneCostStateIsJudgedOnItsOwnTurn(t *testing.T) {
	for _, kind := range []costledger.Kind{costledger.KindTurn, costledger.KindPartial} {
		t.Run(string(kind), func(t *testing.T) {
			s := newReconcileScope(t)
			s.transcript(t, rcSID, scopeMsg(s.day(-2, 10, 0), "msg_1", 10, "sdk-cli"), rcCostState(10, 10000),
				rcLine("queue-operation", s.day(-1, 9, 0), "", 0), scopeMsg(s.day(-1, 9, 1), "msg_2", 8, "sdk-cli"),
				rcLine("queue-operation", s.day(-1, 15, 0), "", 0), scopeMsg(s.day(-1, 15, 1), "msg_3", 2, "sdk-cli"))
			s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-2, 9, 59))
			earlier := rcTurn(s.day(-1, 9, 2), rcKey, "cccccccccccccccc", 18)
			if kind == costledger.KindPartial {
				earlier = rcTurn(s.day(-1, 9, 2), rcKey, "end:"+rcSID+":p1", 8)
				earlier.Kind = kind
			}
			seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-2, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 10),
				earlier, rcTurn(s.day(-1, 15, 2), rcKey, "dddddddddddddddd", 12), rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
			rep, out := s.run(t)
			if n := len(rep.Flagged); n == 0 || rep.Flagged[n-1].Entry.RunID != "dddddddddddddddd" {
				t.Fatalf("flagged %+v, want the last resumed turn among them\n%s", rep.Flagged, out)
			}
		})
	}
}

// Spend in a turn's window that no entry booked, such as a workflow agent's,
// does not hide a restore the entry charged with nothing of its own on top.
func TestReconcile_UnbookedSpendInTheTurnDoesNotHideARestore(t *testing.T) {
	s := newReconcileScope(t)
	s.transcript(t, rcSID, scopeMsg(s.day(-2, 10, 0), "msg_1", 10, "sdk-cli"), rcCostState(10, 10000),
		rcLine("queue-operation", s.day(-1, 9, 0), "", 0), scopeMsg(s.day(-1, 9, 1), "msg_u", 3, "sdk-cli"))
	s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-2, 9, 59))
	seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-2, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 10),
		rcTurn(s.day(-1, 9, 2), rcKey, "cccccccccccccccc", 10), rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
	rep, out := s.run(t)
	if len(rep.Flagged) != 1 || rep.Flagged[0].Entry.RunID != "cccccccccccccccc" {
		t.Fatalf("flagged %+v, want the resumed turn\n%s", rep.Flagged, out)
	}
	if st := settlementOf(rep, rcSID); !near(st.After, 13) {
		t.Errorf("settlement = %+v, want the restore out and the unbooked 3 in: 13\n%s", st, out)
	}
}

// An entry smaller than the restore did not charge it, however little of
// its turn the transcript shows.
func TestReconcile_AnEntryBelowTheRestoreIsNotFlagged(t *testing.T) {
	s := newReconcileScope(t)
	s.transcript(t, rcSID, scopeMsg(s.day(-2, 10, 0), "msg_1", 10, "sdk-cli"), rcCostState(10, 10000),
		rcLine("queue-operation", s.day(-1, 9, 0), "", 0))
	s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-2, 9, 59))
	seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-2, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 10),
		rcTurn(s.day(-1, 9, 2), rcKey, "cccccccccccccccc", 6), rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
	if rep, out := s.run(t); len(rep.Flagged) != 0 {
		t.Fatalf("flagged %+v, want nothing\n%s", rep.Flagged, out)
	}
}

// An entry without model rows is judged on its amount less its turn's priced
// usage; a turn that cannot be priced leaves it unflagged and reported.
func TestReconcile_ARowlessEntryIsJudgedOnItsPricedTurn(t *testing.T) {
	for _, c := range []struct {
		name      string
		amount    float64
		model     string
		flagged   int
		undecided int
	}{
		{"restore plus the turn", 3.6, rcModel, 1, 0},
		{"the turn alone", 3, rcModel, 0, 0},
		{"turn not priced", 3, "claude-mystery-9", 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newReconcileScope(t)
			s.transcript(t, rcSID, scopeMsg(s.day(-2, 10, 0), "msg_1", 0.6, "sdk-cli"), rcCostState(0.6, 600),
				rcLine("queue-operation", s.day(-1, 9, 0), "", 0), rcLine("assistant", s.day(-1, 9, 1), "msg_2", 3000, c.model))
			s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-2, 9, 59))
			rowless := rcTurn(s.day(-1, 9, 2), rcKey, "cccccccccccccccc", c.amount)
			rowless.Models = nil
			seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-2, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 0.6),
				rowless, rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
			rep, out := s.run(t)
			st := settlementOf(rep, rcSID)
			if len(rep.Flagged) != c.flagged || st.UndecidedN != c.undecided {
				t.Fatalf("flagged %d undecided %d, want %d and %d\n%s", len(rep.Flagged), st.UndecidedN, c.flagged, c.undecided, out)
			}
			if c.undecided > 0 && !strings.Contains(out, "1 条无法判定是否计入恢复额") {
				t.Errorf("report does not name the undecided entry\n%s", out)
			}
		})
	}
}

// A turn's usage runs after from up to and including to, without the
// interactive-terminal messages, and is unpriced when a model has no rate.
func TestTurnWindow(t *testing.T) {
	t0 := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	rates := costledger.NewRateBook()
	rates.Observe(costledger.ModelDelta{Model: rcModel, CostUSD: 1, Tokens: costledger.Tokens{CacheRead: 1000}})
	msg := func(min int, cacheRead int64, entrypoint, model string) claudefs.MessageUsage {
		return claudefs.MessageUsage{ModelTokens: claudefs.ModelTokens{Model: model, CacheRead: cacheRead},
			At: t0.Add(time.Duration(min) * time.Minute), Entrypoint: entrypoint}
	}
	msgs := []claudefs.MessageUsage{msg(0, 1, "sdk-cli", rcModel), msg(1, 1000, "sdk-cli", rcModel), msg(2, 2000, "", rcModel),
		msg(3, 4000, "cli", rcModel), msg(4, 8000, "claude-vscode", rcModel), msg(5, 16000, "sdk-cli", rcModel), msg(6, 32000, "sdk-cli", rcModel)}
	if got := turnWindow(msgs, t0, t0.Add(5*time.Minute), rates); got.tokens != 19000 || !got.priced || !near(got.usd, 19) {
		t.Errorf("turnWindow = %+v, want 19000 tokens priced at 19", got)
	}
	msgs = append(msgs, msg(5, 1, "sdk-cli", "claude-mystery-9"))
	if got := turnWindow(msgs, t0, t0.Add(5*time.Minute), rates); got.priced {
		t.Errorf("turnWindow = %+v, want unpriced with a model no row taught", got)
	}
}

// A backfill is booked at its run's start, so its turn is the run, up to the
// run's recorded end or else the next entry, and the turn after it starts
// where the run ended. One that nothing bounds is reported, not flagged.
func TestReconcile_ABackfillIsJudgedOnItsRun(t *testing.T) {
	for _, c := range []struct {
		name         string
		backfill, dd float64 // dd 0 books no later turn
		ended        bool
		flagged      []string
		undecided    int
	}{
		{"baselined backfill", 3, 1, true, nil, 0},
		{"backfill charging the restore", 3.6, 1, true, []string{"cccccccccccccccc"}, 0},
		{"turn after a backfill charging the restore", 3, 1.6, true, []string{"dddddddddddddddd"}, 0},
		{"backfill without an end, bounded by the next entry", 3, 1, false, nil, 0},
		{"backfill nothing bounds", 3, 0, false, nil, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newReconcileScope(t)
			s.transcript(t, rcSID, scopeMsg(s.day(-2, 10, 0), "msg_1", 0.6, "sdk-cli"), rcCostState(0.6, 600),
				rcLine("queue-operation", s.day(-1, 9, 0), "", 0),
				scopeMsg(s.day(-1, 9, 1), "msg_2", 3, "sdk-cli"), scopeMsg(s.day(-1, 10, 1), "msg_3", 1, "sdk-cli"))
			s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-2, 9, 59))
			run := runhistory.SessionRun{RunID: "cccccccccccccccc", SessionKey: rcKey, SessionID: rcSID, StartedAt: s.day(-1, 9, 0)}
			if c.ended {
				run.EndedAt = s.day(-1, 9, 2)
			}
			s.sessionRun(t, run)
			backfill := rcTurn(run.StartedAt, rcKey, run.RunID, c.backfill)
			backfill.Kind, backfill.Models = costledger.KindBackfill, nil
			entries := []costledger.Entry{rcTurn(s.day(-2, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 0.6), backfill,
				rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1)}
			if c.dd > 0 {
				entries = append(entries, rcTurn(s.day(-1, 10, 2), rcKey, "dddddddddddddddd", c.dd))
			}
			seedLedger(t, s.opts.SessionStorePath, entries...)
			rep, out := s.run(t)
			var flagged []string
			for _, f := range rep.Flagged {
				flagged = append(flagged, f.Entry.RunID)
			}
			if strings.Join(flagged, ",") != strings.Join(c.flagged, ",") || settlementOf(rep, rcSID).UndecidedN != c.undecided {
				t.Fatalf("flagged %v undecided %d, want %v and %d\n%s", flagged, settlementOf(rep, rcSID).UndecidedN, c.flagged, c.undecided, out)
			}
		})
	}
}

// An adjustment an earlier -write booked inside a turn, such as a day
// residual at noon, does not cut that turn short.
func TestReconcile_AnEarlierAdjustmentDoesNotSplitATurn(t *testing.T) {
	s := newReconcileScope(t)
	s.transcript(t, rcSID, scopeMsg(s.day(-2, 10, 0), "msg_1", 0.6, "sdk-cli"), rcCostState(0.6, 600),
		rcLine("queue-operation", s.day(-1, 11, 0), "", 0),
		scopeMsg(s.day(-1, 11, 30), "msg_2", 1.5, "sdk-cli"), scopeMsg(s.day(-1, 12, 30), "msg_3", 1.5, "sdk-cli"))
	s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-2, 9, 59))
	residual := rcTurn(s.day(-1, 12, 0), rcKey, reconcilePrefix+rcSID+":day:"+s.day(-1, 0, 0).Format(time.DateOnly), 1)
	residual.Kind, residual.Models = costledger.KindAdjust, nil
	seedLedger(t, s.opts.SessionStorePath, rcTurn(s.day(-2, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 0.6), residual,
		rcTurn(s.day(-1, 12, 31), rcKey, "cccccccccccccccc", 3), rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1))
	if rep, out := s.run(t); len(rep.Flagged) != 0 {
		t.Fatalf("flagged %+v, want nothing\n%s", rep.Flagged, out)
	}
}

// A day the ledger books spend on and the transcript shows none of naozhi's
// is left as booked: nothing proves the entries wrong, and zeroing the day
// would drop spend the transcript no longer holds. A day the transcript shows
// less spend on than booked still gets the difference taken off, and one the
// ledger nets below zero on is raised to zero: spend is never negative.
func TestReconcile_ADayWithNoTranscriptSpendIsLeftAsBooked(t *testing.T) {
	for _, c := range []struct {
		name     string
		lines    func(s reconcileScope) []string
		credit   float64 // an adjustment taken off the $3 day
		residual float64
		adjusts  int
		empty    int
	}{
		{"no lines", func(s reconcileScope) []string { return nil }, 0, 0, 0, 1},
		{"only terminal lines", func(s reconcileScope) []string {
			return []string{scopeMsg(s.day(-1, 9, 0), "msg_term", 4, "cli")}
		}, 0, 0, 0, 1},
		{"less spend than booked", func(s reconcileScope) []string {
			return []string{scopeMsg(s.day(-1, 9, 0), "msg_2", 1, "sdk-cli")}
		}, 0, -2, 1, 0},
		{"no lines and a credit", func(s reconcileScope) []string { return nil }, 5, 2, 1, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newReconcileScope(t)
			s.transcript(t, rcSID, append([]string{scopeMsg(s.day(-2, 10, 0), "msg_1", 5, "sdk-cli")}, c.lines(s)...)...)
			s.runStarted(t, "aaaaaaaaaaaaaaaa", s.day(-2, 9, 59))
			entries := []costledger.Entry{rcTurn(s.day(-2, 10, 1), rcKey, "aaaaaaaaaaaaaaaa", 5),
				rcTurn(s.day(-1, 10, 1), rcKey, "cccccccccccccccc", 3), rcTurn(s.day(-3, 9, 0), scopeKey2, "bbbbbbbbbbbbbbbb", 1)}
			if c.credit > 0 {
				credit := rcTurn(s.day(-1, 10, 1), rcKey, reconcilePrefix+rcSID+":run:cccccccccccccccc", -c.credit)
				credit.Kind, credit.Models = costledger.KindAdjust, nil
				entries = append(entries, credit)
			}
			seedLedger(t, s.opts.SessionStorePath, entries...)
			rep, out := s.run(t)
			var planned float64
			for _, e := range rep.Planned {
				planned += e.Amount
			}
			if len(rep.Planned) != c.adjusts || !near(planned, c.residual) {
				t.Fatalf("planned %+v, want %v in %d adjustment(s)\n%s", rep.Planned, c.residual, c.adjusts, out)
			}
			st := settlementOf(rep, rcSID)
			if want := 8 - c.credit + c.residual; st.EmptyDays != c.empty || !near(st.After, want) {
				t.Errorf("settlement = %+v, want %d empty day(s) and the ledger at %v\n%s", st, c.empty, want, out)
			}
			if named := strings.Contains(out, "天 transcript 无用量"); named != (c.empty == 1) {
				t.Errorf("report names an empty day: %v, want %v\n%s", named, c.empty == 1, out)
			}
		})
	}
}

// A turn under a key that held several sessions, with no run record, goes to
// the session whose last message by the turn's booking is the latest: one
// session of a key runs at a time, and its turn ends just before the result
// is booked. Nothing is guessed when a fork's copied line ties, when no
// session has a message yet, when a session's transcript is gone, or for a
// backfill, which is booked at its run's start.
func TestReconcile_PlacesAChainedKeysTurnByTranscriptTime(t *testing.T) {
	const (
		key = "dashboard:direct:chain:general"
		a   = "44444444-5555-6666-7777-888888888888"
		b   = "55555555-6666-7777-8888-999999999999"
		c   = "66666666-7777-8888-9999-aaaaaaaaaaaa" // no transcript
	)
	turn := func(ts time.Time, runID string, usd float64) costledger.Entry { return rcTurn(ts, key, runID, usd) }
	for _, tc := range []struct {
		name         string
		chain        []string // oldest first
		lines        func(s reconcileScope) map[string][]string
		turns        func(s reconcileScope) []costledger.Entry
		placed       map[string]int
		unattributed int
	}{
		{"the session that ran last", []string{a, b},
			func(s reconcileScope) map[string][]string {
				return map[string][]string{
					a: {scopeMsg(s.day(-1, 9, 0), "msg_a1", 5, "sdk-cli"), scopeMsg(s.day(-1, 11, 0), "msg_a2", 3, "sdk-cli")},
					b: {scopeMsg(s.day(-1, 14, 0), "msg_b", 2, "sdk-cli")}}
			},
			func(s reconcileScope) []costledger.Entry {
				return []costledger.Entry{turn(s.day(-1, 9, 1), "aaaaaaaaaaaaaaa1", 5), turn(s.day(-1, 11, 1), "aaaaaaaaaaaaaaa2", 3),
					turn(s.day(-1, 14, 1), "aaaaaaaaaaaaaaa3", 2)}
			}, map[string]int{a: 2, b: 1}, 0},
		{"a last message stamped after the booking", []string{a, b},
			func(s reconcileScope) map[string][]string {
				return map[string][]string{a: {scopeMsg(s.day(-1, 9, 0), "msg_a1", 5, "sdk-cli")},
					b: {scopeMsg(s.day(-1, 14, 0).Add(5*time.Second), "msg_b", 2, "sdk-cli")}}
			},
			func(s reconcileScope) []costledger.Entry {
				return []costledger.Entry{turn(s.day(-1, 14, 0), "aaaaaaaaaaaaaaa1", 2)}
			}, map[string]int{b: 1}, 0},
		{"a terminal's later line", []string{a, b},
			func(s reconcileScope) map[string][]string {
				return map[string][]string{
					a: {scopeMsg(s.day(-1, 9, 0), "msg_a1", 5, "sdk-cli"), scopeMsg(s.day(-1, 14, 0).Add(30*time.Second), "msg_term", 1, "cli")},
					b: {scopeMsg(s.day(-1, 14, 0), "msg_b", 2, "sdk-cli")}}
			},
			func(s reconcileScope) []costledger.Entry {
				return []costledger.Entry{turn(s.day(-1, 14, 1), "aaaaaaaaaaaaaaa1", 2)}
			}, map[string]int{b: 1}, 0},
		{"a fork's copied line", []string{a, b},
			func(s reconcileScope) map[string][]string {
				parent := scopeMsg(s.day(-1, 9, 0), "msg_a1", 5, "sdk-cli")
				return map[string][]string{a: {parent}, b: {parent, scopeMsg(s.day(-1, 14, 0), "msg_b", 2, "sdk-cli")}}
			},
			func(s reconcileScope) []costledger.Entry {
				return []costledger.Entry{turn(s.day(-1, 9, 1), "aaaaaaaaaaaaaaa1", 5), turn(s.day(-1, 14, 1), "aaaaaaaaaaaaaaa2", 2)}
			}, map[string]int{b: 1}, 1},
		{"before any message", []string{a, b},
			func(s reconcileScope) map[string][]string {
				return map[string][]string{a: {scopeMsg(s.day(-1, 9, 0), "msg_a1", 5, "sdk-cli")},
					b: {scopeMsg(s.day(-1, 14, 0), "msg_b", 2, "sdk-cli")}}
			},
			func(s reconcileScope) []costledger.Entry {
				return []costledger.Entry{turn(s.day(-1, 8, 0), "aaaaaaaaaaaaaaa1", 5)}
			}, nil, 1},
		{"a session without a transcript", []string{a, c, b},
			func(s reconcileScope) map[string][]string {
				return map[string][]string{a: {scopeMsg(s.day(-1, 9, 0), "msg_a1", 5, "sdk-cli")},
					b: {scopeMsg(s.day(-1, 14, 0), "msg_b", 2, "sdk-cli")}}
			},
			func(s reconcileScope) []costledger.Entry {
				return []costledger.Entry{turn(s.day(-1, 14, 1), "aaaaaaaaaaaaaaa1", 2)}
			}, nil, 1},
		{"a backfill", []string{a, b},
			func(s reconcileScope) map[string][]string {
				return map[string][]string{a: {scopeMsg(s.day(-1, 9, 0), "msg_a1", 5, "sdk-cli")},
					b: {scopeMsg(s.day(-1, 14, 0), "msg_b", 2, "sdk-cli")}}
			},
			func(s reconcileScope) []costledger.Entry {
				e := turn(s.day(-1, 14, 1), "aaaaaaaaaaaaaaa1", 2)
				e.Kind, e.Models = costledger.KindBackfill, nil
				return []costledger.Entry{e}
			}, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newReconcileScope(t)
			last := len(tc.chain) - 1
			writeJSON(t, s.opts.SessionStorePath, []map[string]any{
				{"key": rcKey, "session_id": rcSID}, {"key": key, "session_id": tc.chain[last], "prev_session_ids": tc.chain[:last]},
			})
			for sid, lines := range tc.lines(s) {
				s.transcript(t, sid, lines...)
			}
			seedLedger(t, s.opts.SessionStorePath, tc.turns(s)...)
			rep, out := s.run(t)
			if rep.Unattributed != tc.unattributed {
				t.Errorf("unattributed = %d, want %d\n%s", rep.Unattributed, tc.unattributed, out)
			}
			for _, sid := range []string{a, b, c} {
				if got := settlementOf(rep, sid).Entries; got != tc.placed[sid] {
					t.Errorf("%.8s holds %d entries, want %d\n%s", sid, got, tc.placed[sid], out)
				}
			}
		})
	}
}
