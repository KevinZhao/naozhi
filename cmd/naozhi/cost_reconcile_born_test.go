package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

const (
	bornKey  = "dashboard:direct:2026-09-08-115051-3-Workspace:general" // deleted: not in sessions.json
	bornKey2 = "dashboard:direct:2026-09-08-115052-4-Workspace:general"
	bornX    = "77777777-8888-9999-aaaa-bbbbbbbbbbbb"
	bornY    = "88888888-9999-aaaa-bbbb-cccccccccccc"
	bornRun  = "bbbbbbbbbbbbbbb1"
)

// bornLines is a naozhi-started transcript: a queue line at begin, carrying
// no entrypoint, then the prompt naming entrypoint and an assistant line
// of usd 30s later.
func bornLines(begin time.Time, entrypoint, msgID string, usd float64) []string {
	q, _ := json.Marshal(map[string]any{"type": "queue-operation", "operation": "enqueue", "timestamp": begin.Format(time.RFC3339Nano)})
	u, _ := json.Marshal(map[string]any{"type": "user", "timestamp": begin.Add(time.Second).Format(time.RFC3339Nano), "entrypoint": entrypoint})
	return []string{string(q), string(u), scopeMsg(begin.Add(30*time.Second), msgID, usd, entrypoint)}
}

// unnamedRun writes a session-runs record for runID under key with no
// session id, as finishRun wrote a new session's first turn.
func (s reconcileScope) unnamedRun(t *testing.T, runID, key string, from, to time.Time) {
	t.Helper()
	s.sessionRun(t, runhistory.SessionRun{RunID: runID, SessionKey: key, StartedAt: from, EndedAt: to})
}

func (s reconcileScope) knownIDs(t *testing.T, ids ...string) {
	t.Helper()
	writeJSON(t, datadir.ForStore(s.opts.SessionStorePath).SessionIDsPath(), ids)
}

// A deleted key's first turn, whose run record names no session, goes to
// the one naozhi session whose transcript began within that run (5s slack
// either side), and its day settles. Two such sessions, a beginning that
// could not be read, or one another unnamed run also holds name none; a
// session naozhi never ran or one begun in a terminal is no candidate.
func TestReconcile_PlacesAnUnnamedFirstTurnByTranscriptBirth(t *testing.T) {
	const (
		unknown = "99999999-aaaa-bbbb-cccc-dddddddddddd" // not in session-ids.json
		noStamp = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)
	type setup struct {
		s          reconcileScope
		start, end time.Time
	}
	for _, tc := range []struct {
		name         string
		arrange      func(t *testing.T, x setup) costledger.Entry // returns the first turn's entry
		placed       int                                          // entries bornX holds
		unattributed int
		placedY      int
	}{
		{"the session begun in the run", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornX)
			x.s.transcript(t, bornX, bornLines(x.start.Add(time.Second), "sdk-cli", "msg_x", 5)...)
			return rcTurn(x.end, bornKey, bornRun, 2)
		}, 1, 0, 0},
		{"a backfill of that run", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornX)
			x.s.transcript(t, bornX, bornLines(x.start.Add(time.Second), "sdk-cli", "msg_x", 5)...)
			e := rcTurn(x.start, bornKey, bornRun, 2)
			e.Kind, e.Models = costledger.KindBackfill, nil
			return e
		}, 1, 0, 0},
		{"begun within the slack after the run", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornX)
			x.s.transcript(t, bornX, bornLines(x.end.Add(4*time.Second), "sdk-cli", "msg_x", 5)...)
			return rcTurn(x.end, bornKey, bornRun, 2)
		}, 1, 0, 0},
		{"begun past the slack after the run", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornX)
			x.s.transcript(t, bornX, bornLines(x.end.Add(6*time.Second), "sdk-cli", "msg_x", 5)...)
			return rcTurn(x.end, bornKey, bornRun, 2)
		}, 0, 1, 0},
		{"begun past the slack before the run", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornX)
			x.s.transcript(t, bornX, bornLines(x.start.Add(-6*time.Second), "sdk-cli", "msg_x", 5)...)
			return rcTurn(x.end, bornKey, bornRun, 2)
		}, 0, 1, 0},
		{"the key's next first turn, an hour on", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornX, bornY)
			x.s.transcript(t, bornX, bornLines(x.start.Add(time.Second), "sdk-cli", "msg_x", 5)...)
			next := x.start.Add(time.Hour)
			x.s.transcript(t, bornY, bornLines(next.Add(time.Second), "sdk-cli", "msg_y", 1)...)
			x.s.unnamedRun(t, "bbbbbbbbbbbbbbb2", bornKey, next, next.Add(time.Minute))
			seedLedger(t, x.s.opts.SessionStorePath, rcTurn(next.Add(time.Minute), bornKey, "bbbbbbbbbbbbbbb2", 1))
			return rcTurn(x.end, bornKey, bornRun, 2)
		}, 1, 0, 1},
		{"a second session begun in the run", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornX, bornY)
			x.s.transcript(t, bornX, bornLines(x.start.Add(time.Second), "sdk-cli", "msg_x", 5)...)
			x.s.transcript(t, bornY, bornLines(x.start.Add(20*time.Second), "sdk-cli", "msg_y", 1)...)
			return rcTurn(x.end, bornKey, bornRun, 2)
		}, 0, 1, 0},
		{"a session naozhi never ran begun in the run", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornX)
			x.s.transcript(t, bornX, bornLines(x.start.Add(time.Second), "sdk-cli", "msg_x", 5)...)
			x.s.transcript(t, unknown, bornLines(x.start.Add(20*time.Second), "sdk-cli", "msg_u", 1)...)
			return rcTurn(x.end, bornKey, bornRun, 2)
		}, 1, 0, 0},
		{"only a session naozhi never ran", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornY)
			x.s.transcript(t, unknown, bornLines(x.start.Add(time.Second), "sdk-cli", "msg_u", 1)...)
			return rcTurn(x.end, bornKey, bornRun, 2)
		}, 0, 1, 0},
		{"a session begun in a terminal", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornX, bornY)
			x.s.transcript(t, bornX, bornLines(x.start.Add(time.Second), "sdk-cli", "msg_x", 5)...)
			x.s.transcript(t, bornY, bornLines(x.start.Add(20*time.Second), "cli", "msg_y", 1)...)
			return rcTurn(x.end, bornKey, bornRun, 2)
		}, 1, 0, 0},
		{"a beginning not read", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornX, noStamp)
			x.s.transcript(t, bornX, bornLines(x.start.Add(time.Second), "sdk-cli", "msg_x", 5)...)
			x.s.transcript(t, noStamp, `{"type":"summary"}`)
			return rcTurn(x.end, bornKey, bornRun, 2)
		}, 0, 1, 0},
		{"a beginning another unnamed run holds", func(t *testing.T, x setup) costledger.Entry {
			x.s.knownIDs(t, bornX)
			x.s.transcript(t, bornX, bornLines(x.start.Add(time.Second), "sdk-cli", "msg_x", 5)...)
			// A concurrent first turn whose own transcript is gone.
			x.s.unnamedRun(t, "bbbbbbbbbbbbbbb2", bornKey2, x.start.Add(-time.Minute), x.end)
			seedLedger(t, x.s.opts.SessionStorePath, rcTurn(x.end.Add(time.Second), bornKey2, "bbbbbbbbbbbbbbb2", 1))
			return rcTurn(x.end, bornKey, bornRun, 2)
		}, 0, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newReconcileScope(t)
			x := setup{s: s, start: s.day(-1, 10, 0)}
			x.end = x.start.Add(2 * time.Minute)
			s.unnamedRun(t, bornRun, bornKey, x.start, x.end)
			seedLedger(t, s.opts.SessionStorePath, tc.arrange(t, x))
			rep, out := s.run(t)
			if rep.Unattributed != tc.unattributed {
				t.Errorf("unattributed = %d, want %d\n%s", rep.Unattributed, tc.unattributed, out)
			}
			for sid, want := range map[string]int{bornX: tc.placed, bornY: tc.placedY} {
				if got := settlementOf(rep, sid).Entries; got != want {
					t.Errorf("%.8s holds %d entries, want %d\n%s", sid, got, want, out)
				}
			}
		})
	}
}

// The placed first turn settles its day: its record's start bounds the
// session's history, so the turn's own lines before its booking are not
// held as earlier history, and the $3 the transcript shows over the $2
// booked is planned. -session plans the same for that session.
func TestReconcile_APlacedUnnamedFirstTurnSettlesItsDay(t *testing.T) {
	for _, only := range []string{"", bornX} {
		s := newReconcileScope(t)
		s.opts.Session = only
		start := s.day(-1, 10, 0)
		end := start.Add(2 * time.Minute)
		s.unnamedRun(t, bornRun, bornKey, start, end)
		s.knownIDs(t, bornX)
		s.transcript(t, bornX, bornLines(start.Add(time.Second), "sdk-cli", "msg_x", 5)...)
		seedLedger(t, s.opts.SessionStorePath, rcTurn(end, bornKey, bornRun, 2))
		rep, out := s.run(t)
		st := settlementOf(rep, bornX)
		if st.OpenDays != 0 || st.Entries != 1 {
			t.Errorf("session=%q: settlement %+v, want its one entry and no open day\n%s", only, st, out)
		}
		var planned []costledger.Entry
		for _, e := range rep.Planned {
			if e.RunID == reconcilePrefix+bornX+":day:"+start.Format(time.DateOnly) {
				planned = append(planned, e)
			}
		}
		if len(planned) != 1 || !near(planned[0].Amount, 3) || planned[0].SessionKey != bornKey {
			t.Errorf("session=%q: planned %+v, want one +3 day residual under %s\n%s", only, rep.Planned, bornKey, out)
		}
	}
}
