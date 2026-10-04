package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

const (
	rcSID   = "980ad469-9020-4c47-9780-fc46edadf648"
	rcLost  = "11111111-2222-3333-4444-555555555555" // ledger entries, no transcript
	rcKey   = "dashboard:direct:r6ef6dd8c4938c444344f61c0d78c63a9:general"
	rcModel = "claude-opus-5-5"
	// rcRenamed is a key the session store no longer holds.
	rcRenamed = "dashboard:direct:renamed:general"
)

// reconcileFixture is a session whose ledger reproduces the restored-history
// over-charge: a first day of genuine spend that ends in a cost-state of
// $929.98, then two resumed processes on the next day that each charged that
// total again on top of their own turn ($30 and $20), plus a workflow agent's
// $50 no result ever booked and a $5 turn no Send owned, booked under a key
// renamed away with no run record of its own. Prices are $1 per 1000
// cache-read tokens, which every CLI-priced row in the ledger teaches.
type reconcileFixture struct {
	opts            reconcileOpts
	d1, d2, d3      time.Time
	e2, e3, partial costledger.Entry
}

func newReconcileFixture(t *testing.T) reconcileFixture {
	t.Helper()
	dir := t.TempDir()
	until := time.Now().UTC().Truncate(24 * time.Hour)
	f := reconcileFixture{d1: until.Add(-72 * time.Hour), d2: until.Add(-48 * time.Hour), d3: until.Add(-24 * time.Hour)}
	f.opts = reconcileOpts{SessionStorePath: filepath.Join(dir, "sessions.json"), ClaudeDir: filepath.Join(dir, "claude"), Until: until}
	at := func(day time.Time, h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }

	proj := filepath.Join(claudefs.ProjectsRoot(f.opts.ClaudeDir), "-w-naozhi")
	writeRaw(t, claudefs.TranscriptIn(proj, rcSID),
		rcLine("queue-operation", at(f.d1, 9), "", 0),
		rcLine("assistant", at(f.d1, 10), "msg_1", 929980),
		rcCostState(929.98, 929980),
		rcLine("queue-operation", at(f.d2, 9), "", 0),
		rcLine("assistant", at(f.d2, 9).Add(time.Minute), "msg_2", 30000),
		rcLine("queue-operation", at(f.d2, 15), "", 0),
		rcLine("assistant", at(f.d2, 15).Add(time.Minute), "msg_3", 20000),
		rcLine("assistant", at(f.d2, 16).Add(time.Minute), "msg_5", 5000),
		rcLine("assistant", at(f.d3, 10), "msg_x", 5000, "claude-mystery-9"), // a model no row priced
	)
	writeRaw(t, filepath.Join(claudefs.SubagentsDir(proj, rcSID), "workflows", "wf_1", "agent-a1.jsonl"),
		rcLine("assistant", at(f.d2, 16), "msg_4", 50000))

	writeJSON(t, f.opts.SessionStorePath, []map[string]any{
		{"key": rcKey, "session_id": rcSID},
		{"key": "dashboard:direct:chained:general", "session_id": "s-new", "prev_session_ids": []string{"s-old"}},
	})
	runs := datadir.ForStore(f.opts.SessionStorePath).SessionRunsRoot()
	writeJSON(t, filepath.Join(runs, "abcd", "1111111111111111.json"), runhistory.SessionRun{RunID: "1111111111111111", SessionKey: rcKey, SessionID: rcSID})
	writeJSON(t, filepath.Join(runs, "abcd", "3333333333333333.json"), runhistory.SessionRun{RunID: "3333333333333333", SessionKey: rcKey, SessionID: rcSID})
	writeJSON(t, filepath.Join(runs, "abcd", "7777777777777777.json"), runhistory.SessionRun{RunID: "7777777777777777", SessionKey: "dashboard:direct:lost:general", SessionID: rcLost})
	writeJSON(t, filepath.Join(runs, "abcd", "8888888888888888.json"), runhistory.SessionRun{RunID: "8888888888888888", SessionKey: rcRenamed, SessionID: rcSID})

	turn := func(ts time.Time, runID string, usd float64) costledger.Entry { return rcTurn(ts, rcKey, runID, usd) }
	e1 := turn(at(f.d1, 10).Add(time.Minute), "1111111111111111", 929.98)
	f.e2 = turn(at(f.d2, 9).Add(2*time.Minute), "2222222222222222", 959.98) // no run record: the key holds only this session
	f.e3 = turn(at(f.d2, 15).Add(2*time.Minute), "3333333333333333", 949.98)
	// Only its run id names the session: its key is gone from the store.
	f.partial = costledger.Entry{TS: at(f.d2, 17), Source: costledger.SourceSession, Kind: costledger.KindPartial, SessionKey: rcRenamed,
		RunID: "end:" + rcSID + ":p1", Backend: "claude", Unit: costledger.UnitUSD, Amount: 10,
		Models: []costledger.ModelDelta{{Model: rcModel, CostUSD: 10, Tokens: costledger.Tokens{CacheRead: 10000}}}}
	today := turn(until.Add(time.Hour), "4444444444444444", 929.98)         // today: left alone
	chained := turn(at(f.d2, 1), "5555555555555555", 3)                     // key held two sessions: unattributed
	cron := turn(at(f.d2, 2), "6666666666666666", 929.98)                   // cron books its own
	lost := turn(at(f.d2, 3), "7777777777777777", 2)                        // its transcript is gone
	unowned := turn(at(f.d2, 16).Add(2*time.Minute), "9999999999999999", 5) // only its key's run records name the session
	chained.SessionKey, cron.SessionKey, lost.SessionKey = "dashboard:direct:chained:general", "cron:0123456789abcdef", "dashboard:direct:lost:general"
	unowned.SessionKey = rcRenamed
	f.seed(t, e1, f.e2, f.e3, f.partial, today, chained, cron, lost, unowned)
	return f
}

// rcTurn is a CLI-priced turn under key at $1 per 1000 cache-read tokens.
func rcTurn(ts time.Time, key, runID string, usd float64) costledger.Entry {
	return costledger.Entry{TS: ts, Source: costledger.SourceSession, Kind: costledger.KindTurn, SessionKey: key,
		RunID: runID, Workspace: "naozhi", Backend: "claude", Unit: costledger.UnitUSD, Amount: usd, Basis: costledger.BasisList,
		Models: []costledger.ModelDelta{{Model: rcModel, RawModel: rcModel + "[1m]", Basis: costledger.BasisList, CostUSD: usd, Tokens: costledger.Tokens{CacheRead: int64(math.Round(usd * 1000))}}}}
}

func (f reconcileFixture) seed(t *testing.T, entries ...costledger.Entry) {
	t.Helper()
	seedLedger(t, f.opts.SessionStorePath, entries...)
}

func seedLedger(t *testing.T, storePath string, entries ...costledger.Entry) {
	t.Helper()
	store := costledger.NewStore(datadir.ForStore(storePath).CostRoot(), costledger.Options{})
	defer store.Close()
	for _, e := range entries {
		if !store.Append(e) {
			t.Fatalf("seed %s rejected", e.RunID)
		}
	}
}

func rcLine(typ string, ts time.Time, id string, cacheRead int64, model ...string) string {
	v := map[string]any{"type": typ, "timestamp": ts.Format(time.RFC3339Nano), "sessionId": rcSID, "entrypoint": "sdk-cli"}
	if typ == "assistant" {
		m := rcModel
		if len(model) > 0 {
			m = model[0]
		}
		v["message"] = map[string]any{"id": id, "model": m, "role": "assistant",
			"usage": map[string]any{"cache_read_input_tokens": cacheRead}}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func rcCostState(usd float64, cacheRead int64) string {
	b, _ := json.Marshal(map[string]any{"type": "cost-state", "sessionId": rcSID, "totalCostUSD": usd,
		"modelUsage": map[string]any{"global.anthropic." + rcModel + "[1m]": map[string]any{"cacheReadInputTokens": cacheRead, "costUSD": usd}}})
	return string(b)
}

func writeRaw(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ledgerSnapshot is every day file's bytes, to prove a run wrote nothing.
func ledgerSnapshot(t *testing.T, storePath string) map[string]string {
	t.Helper()
	out := map[string]string{}
	files, _ := filepath.Glob(filepath.Join(datadir.ForStore(storePath).CostRoot(), "*"))
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = string(b)
	}
	return out
}

func sessionTotal(t *testing.T, storePath string, from, to time.Time) float64 {
	t.Helper()
	store := costledger.OpenReadOnly(datadir.ForStore(storePath).CostRoot(), costledger.Options{})
	defer store.Close()
	sum, err := store.Summarize(costledger.Query{From: from, To: to, GroupBy: costledger.GroupBySession})
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, b := range sum.Buckets {
		if b.Key == rcKey || b.Key == rcRenamed {
			total += b.Amount
		}
	}
	return total
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// The dry run plans the two restored totals back out and the day's
// unbooked transcript spend in, and writes nothing.
func TestReconcile_DryRunPlansTheDoubleRestoreAndWritesNothing(t *testing.T) {
	f := newReconcileFixture(t)
	before := ledgerSnapshot(t, f.opts.SessionStorePath)
	var out bytes.Buffer
	rep, err := reconcileLedger(f.opts, &out)
	if err != nil {
		t.Fatal(err)
	}
	if got := ledgerSnapshot(t, f.opts.SessionStorePath); len(got) != len(before) || !mapsEqual(got, before) {
		t.Fatalf("dry run changed the ledger:\nbefore %v\nafter  %v", before, got)
	}
	if len(rep.Flagged) != 2 || rep.Flagged[0].Entry.RunID != f.e2.RunID || rep.Flagged[1].Entry.RunID != f.e3.RunID {
		t.Fatalf("flagged = %+v, want the two resumed turns\n%s", rep.Flagged, out.String())
	}
	if len(rep.Planned) != 3 {
		t.Fatalf("planned %d adjustments, want 3\n%s", len(rep.Planned), out.String())
	}
	for i, e := range []costledger.Entry{f.e2, f.e3} {
		adj := rep.Planned[i]
		if adj.Kind != costledger.KindAdjust || !near(adj.Amount, -929.98) || !adj.TS.Equal(e.TS) || adj.SessionKey != rcKey ||
			adj.RunID != "reconcile:"+rcSID+":run:"+e.RunID {
			t.Errorf("flag adjust %d = %+v", i, adj)
		}
		if len(adj.Models) != 1 || adj.Models[0].Model != rcModel || adj.Models[0].CacheRead != -929980 || !near(adj.Models[0].CostUSD, -929.98) {
			t.Errorf("flag adjust %d rows = %+v, want the restored row negated under the entry's model", i, adj.Models)
		}
	}
	// Day two: transcript 30+20+5+50 = 105 against 959.98+949.98+5+10
	// booked, less the two restores: 65 booked, so 40 is missing. It is
	// booked under the day's last key, not the session's first.
	res := rep.Planned[2]
	if !near(res.Amount, 40) || res.RunID != "reconcile:"+rcSID+":day:"+f.d2.Format(time.DateOnly) || res.TS.Format(time.DateOnly) != f.d2.Format(time.DateOnly) {
		t.Errorf("residual = %+v, want +40 on day two", res)
	}
	if res.SessionKey != rcRenamed {
		t.Errorf("residual booked under %q, want the day's last key %q", res.SessionKey, rcRenamed)
	}
	if len(res.Models) != 1 || math.Abs(res.Models[0].CostUSD-40) > 1e-6 || res.Models[0].CacheRead != 40000 {
		t.Errorf("residual rows = %+v, want +40 / 40000 cache-read", res.Models)
	}
	var s sessionSettlement
	for _, st := range rep.Sessions {
		if st.SessionID == rcSID {
			s = st
		}
		if st.SessionID == rcLost && st.Skipped == "" {
			t.Errorf("session without a transcript was settled: %+v", st)
		}
	}
	if s.UnpricedDays != 1 || !near(s.Transcript, 1034.98) || !near(s.After, 1034.98) {
		t.Errorf("settlement = %+v, want after = transcript = 1034.98 and day three unpriced", s)
	}
	if rep.Unattributed != 1 {
		t.Errorf("unattributed = %d, want 1 (the key with a session chain)", rep.Unattributed)
	}
}

// Written, the session's ledger total matches its transcripts over the
// settled days, and a second run finds nothing left to do.
func TestReconcile_WriteSettlesAndASecondRunAppendsNothing(t *testing.T) {
	f := newReconcileFixture(t)
	f.opts.Write = true
	rep, err := reconcileLedger(f.opts, &bytes.Buffer{})
	if err != nil || rep.Appended != 3 {
		t.Fatalf("appended %d err=%v, want 3", rep.Appended, err)
	}
	if got := sessionTotal(t, f.opts.SessionStorePath, f.d1, f.opts.Until); !near(got, 1034.98) {
		t.Errorf("ledger total for the settled days = %v, want the transcript's 1034.98", got)
	}
	// The drill-down nets out to the transcript under the model the turns
	// were booked as, with no bucket named after the cost-state's key.
	ro := costledger.OpenReadOnly(datadir.ForStore(f.opts.SessionStorePath).CostRoot(), costledger.Options{})
	defer ro.Close()
	models := map[string]costledger.Bucket{}
	for _, key := range []string{rcKey, rcRenamed} {
		sum, err := ro.Summarize(costledger.Query{From: f.d1, To: f.opts.Until, SessionKey: key, GroupBy: costledger.GroupByModel})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range sum.Buckets {
			m := models[b.Key]
			m.Amount += b.Amount
			m.Tokens.CacheRead += b.Tokens.CacheRead
			models[b.Key] = m
		}
	}
	if m := models[rcModel]; len(models) != 1 || !near(m.Amount, 1034.98) || m.Tokens.CacheRead != 1034980 {
		t.Errorf("model buckets %+v, want only %s at the transcript's 1034.98 / 1034980 cache-read", models, rcModel)
	}
	if got := sessionTotal(t, f.opts.SessionStorePath, f.opts.Until, f.opts.Until.Add(24*time.Hour)); !near(got, 929.98) {
		t.Errorf("today's total = %v, want it untouched", got)
	}
	before := ledgerSnapshot(t, f.opts.SessionStorePath)
	var out bytes.Buffer
	rep, err = reconcileLedger(f.opts, &out)
	if err != nil || len(rep.Planned) != 0 || rep.Appended != 0 {
		t.Fatalf("second run planned %d appended %d err=%v, want nothing\n%s", len(rep.Planned), rep.Appended, err, out.String())
	}
	if !mapsEqual(ledgerSnapshot(t, f.opts.SessionStorePath), before) {
		t.Error("second run changed the ledger")
	}
}

// A day on which an entry no session could be named for shares a key with
// the session's own entries gets no residual: that entry's spend may be in
// the session's transcript and is already in the ledger.
func TestReconcile_HoldsADayAnUnattributedEntryMayBelongTo(t *testing.T) {
	f := newReconcileFixture(t)
	const chained = "dashboard:direct:chained:general"
	writeJSON(t, filepath.Join(datadir.ForStore(f.opts.SessionStorePath).SessionRunsRoot(), "abcd", "aaaaaaaaaaaaaaaa.json"),
		runhistory.SessionRun{RunID: "aaaaaaaaaaaaaaaa", SessionKey: chained, SessionID: rcSID})
	f.seed(t, costledger.Entry{TS: f.d2.Add(4 * time.Hour), Source: costledger.SourceSession, Kind: costledger.KindTurn, SessionKey: chained,
		RunID: "aaaaaaaaaaaaaaaa", Backend: "claude", Unit: costledger.UnitUSD, Amount: 1})
	var out bytes.Buffer
	rep, err := reconcileLedger(f.opts, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Flagged) != 2 || len(rep.Planned) != 2 {
		t.Fatalf("flagged %d planned %d, want the two flags and no residual\n%s", len(rep.Flagged), len(rep.Planned), out.String())
	}
	for _, st := range rep.Sessions {
		if st.SessionID == rcSID && st.HeldDays != 1 {
			t.Errorf("held days = %d, want 1\n%s", st.HeldDays, out.String())
		}
	}
}

// A flag's rows land in the flagged entry's own model buckets, whatever the
// cost-state calls the model, take no more than the entry's row carried, and
// an entry booked without rows gets none.
func TestRestoreRows(t *testing.T) {
	m := claudefs.CostStateMark{CostState: claudefs.CostState{TotalCostUSD: 584.17, ModelUsage: json.RawMessage(
		`{"global.anthropic.claude-opus-5[1m]":{"cacheReadInputTokens":584170,"outputTokens":900,"costUSD":584.17}}`)}}
	e := costledger.Entry{Amount: 600, Models: []costledger.ModelDelta{{Model: "claude-opus-5", RawModel: "global.anthropic.claude-opus-5[1m]",
		Provider: "bedrock", Basis: costledger.BasisList, CostUSD: 600, Tokens: costledger.Tokens{CacheRead: 600000, Output: 500}}}}
	got := restoreRows(e, m)
	want := costledger.ModelDelta{Model: "claude-opus-5", RawModel: "global.anthropic.claude-opus-5[1m]", Provider: "bedrock",
		Basis: costledger.BasisList, CostUSD: -584.17, Tokens: costledger.Tokens{CacheRead: -584170, Output: -500}}
	if len(got) != 1 || got[0] != want {
		t.Errorf("restoreRows = %+v, want [%+v]", got, want)
	}
	if got := restoreRows(costledger.Entry{Amount: 600}, m); got != nil {
		t.Errorf("row-less entry got rows %+v", got)
	}
}

// -session settles only that session, and one with no ledger entries is an
// error rather than an empty report.
func TestReconcile_SessionFilter(t *testing.T) {
	f := newReconcileFixture(t)
	f.opts.Session = rcLost
	rep, err := reconcileLedger(f.opts, &bytes.Buffer{})
	if err != nil || len(rep.Sessions) != 1 || rep.Sessions[0].SessionID != rcLost || len(rep.Planned) != 0 {
		t.Fatalf("sessions = %+v planned=%d err=%v", rep.Sessions, len(rep.Planned), err)
	}
	f.opts.Session = "aaaaaaaa-0000-0000-0000-000000000000"
	if _, err := reconcileLedger(f.opts, &bytes.Buffer{}); err == nil {
		t.Error("a session with no entries must be an error")
	}
}

// An entry charged a restore only when the cost-state was already behind it
// when its process started, and only when its rows carry the restored
// tokens: a correctly baselined turn as large as the restore is genuine.
func TestChargesRestore(t *testing.T) {
	ts := time.Date(2026, 9, 28, 4, 33, 46, 0, time.UTC)
	marks := []claudefs.CostStateMark{
		{CostState: claudefs.CostState{TotalCostUSD: 584.17, ModelUsage: json.RawMessage(`{"m":{"cacheReadInputTokens":584170,"costUSD":584.17}}`)},
			After: ts.Add(-time.Hour)},
		{CostState: claudefs.CostState{TotalCostUSD: 614.54}, Before: ts.Add(-time.Second), After: ts.Add(time.Hour)}, // written by this process
	}
	m, ok := restoredBy(marks, ts)
	if !ok || m.TotalCostUSD != 584.17 {
		t.Fatalf("restoredBy = %v %v, want the 584.17 written before the process", m.TotalCostUSD, ok)
	}
	row := func(cacheRead int64) []costledger.ModelDelta {
		return []costledger.ModelDelta{{Model: "m", Tokens: costledger.Tokens{CacheRead: cacheRead}}}
	}
	for _, c := range []struct {
		name string
		e    costledger.Entry
		want bool
	}{
		{"restore plus a turn", costledger.Entry{Amount: 614.54, Models: row(614540)}, true},
		{"no rows: amount alone", costledger.Entry{Amount: 600}, true},
		{"large turn, baselined", costledger.Entry{Amount: 600, Models: row(60000)}, false},
		{"below the restore", costledger.Entry{Amount: 500, Models: row(614540)}, false},
	} {
		if got := chargesRestore(c.e, m); got != c.want {
			t.Errorf("%s: chargesRestore = %v, want %v", c.name, got, c.want)
		}
	}
	small := claudefs.CostStateMark{CostState: claudefs.CostState{TotalCostUSD: 0.4}}
	if chargesRestore(costledger.Entry{Amount: 0.5}, small) {
		t.Error("a restore under the floor flagged an entry")
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
