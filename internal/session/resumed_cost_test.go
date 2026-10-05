package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
)

const resumedSID = "980ad469-9020-4c47-9780-fc46edadf648"

// resumeRouter returns a router whose next GetOrCreate of key resumes a dead
// session (resumedSID, workspace ws, costSpent 10) whose transcript holds
// lines, and whose spawn hands back a process reporting first.
func resumeRouter(t *testing.T, key string, first *clievent.SendResult, lines ...string) (*Router, *costledger.Store, string) {
	t.Helper()
	claudeDir, ws := t.TempDir(), t.TempDir()
	r := NewRouter(RouterConfig{
		Wrapper:   cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude"),
		ClaudeDir: claudeDir,
	})
	t.Cleanup(r.Shutdown)
	ledger := costledger.NewStore(t.TempDir(), costledger.Options{})
	t.Cleanup(ledger.Close)
	r.runs.cost = newCostAccounting(ledger)

	path := claudefs.SessionJSONL(claudeDir, ws, resumedSID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	dead := &ManagedSession{key: key}
	dead.SetBackend("claude")
	dead.setWorkspace(ws)
	dead.setSessionID(resumedSID)
	storeTotalCost(&dead.costSpent, 10)
	putT(r, key, dead)

	r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
		if opts.ResumeID != resumedSID {
			t.Errorf("spawn ResumeID = %q, want %q (the test needs a resume)", opts.ResumeID, resumedSID)
		}
		return &TestProcess{AliveVal: true, SendFunc: scripted(first)}, nil
	}
	return r, ledger, path
}

func restoredCostLine(usd float64) string {
	b, _ := json.Marshal(map[string]any{
		"type": "cost-state", "sessionId": resumedSID, "totalCostUSD": usd,
		"modelUsage": map[string]any{"claude-opus-5-5[1m]": map[string]any{"outputTokens": 1000, "costUSD": usd}},
	})
	return string(b)
}

func resumeAndSend(t *testing.T, r *Router, key string) *ManagedSession {
	t.Helper()
	s, status, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if status != SessionResumed {
		t.Fatalf("status = %v, want SessionResumed", status)
	}
	if _, err := s.Send(context.Background(), "hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	return s
}

// #3096: a resumed claude reports the cost it restored from the transcript's
// cost-state plus the new turn. Only the new turn is this session's spend;
// before the fix the restored 929.98 was charged again on every respawn.
func TestResumedCost_FirstTurnChargesOnlyItsOwnSpend(t *testing.T) {
	const key = "dashboard:direct:resumed:general"
	first := &clievent.SendResult{Text: "a", CostUSD: 930.48, ModelUsage: map[string]clievent.ModelUsage{
		"claude-opus-5-5[1m]": {OutputTokens: 1100, CostUSD: 930.48},
	}}
	r, ledger, _ := resumeRouter(t, key, first,
		`{"type":"user","sessionId":"`+resumedSID+`"}`,
		restoredCostLine(929.98),
		`{"type":"assistant","sessionId":"`+resumedSID+`"}`,
	)

	s := resumeAndSend(t, r, key)

	if got := loadTotalCost(&s.costSpent); !approxEq(got, 10.5) {
		t.Fatalf("costSpent = %v, want 10.5 (10 carried + the 0.5 this turn spent, not the restored 929.98)", got)
	}
	ents := allEntries(t, ledger)
	if len(ents) != 1 || !approxEq(ents[0].Amount, 0.5) {
		t.Fatalf("ledger = %+v, want one 0.5 entry", ents)
	}
	if len(ents[0].Models) != 1 || ents[0].Models[0].Tokens.Output != 100 {
		t.Errorf("model rows = %+v, want the 100 output tokens of this turn", ents[0].Models)
	}
}

// Control: a transcript with no cost-state restores nothing, so the first
// reading IS the turn and is charged in full. Without this the test above
// would also pass with "never charge the first turn after a resume".
func TestResumedCost_NoCostStateChargesTheFirstTurnInFull(t *testing.T) {
	const key = "dashboard:direct:resumed-none:general"
	r, ledger, _ := resumeRouter(t, key, &clievent.SendResult{Text: "a", CostUSD: 0.7},
		`{"type":"user","sessionId":"`+resumedSID+`"}`)

	s := resumeAndSend(t, r, key)

	if got := loadTotalCost(&s.costSpent); !approxEq(got, 10.7) {
		t.Fatalf("costSpent = %v, want 10.7", got)
	}
	if ents := allEntries(t, ledger); len(ents) != 1 || !approxEq(ents[0].Amount, 0.7) {
		t.Fatalf("ledger = %+v, want one 0.7 entry", ents)
	}
}

// An unreadable transcript may still hold a cost-state the CLI restores, so
// the first reading is adopted as the baseline: that turn charges nothing
// rather than risking the history again.
func TestResumedCost_UnreadableTranscriptChargesNothingRatherThanHistory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0o000 file")
	}
	const key = "dashboard:direct:resumed-unreadable:general"
	r, ledger, path := resumeRouter(t, key, &clievent.SendResult{Text: "a", CostUSD: 930.48},
		restoredCostLine(929.98))
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	s := resumeAndSend(t, r, key)

	if got := loadTotalCost(&s.costSpent); !approxEq(got, 10) {
		t.Fatalf("costSpent = %v, want 10 (the unknown-baseline turn charges nothing)", got)
	}
	if ents := allEntries(t, ledger); len(ents) != 0 {
		t.Fatalf("ledger = %+v, want no entries", ents)
	}
}
