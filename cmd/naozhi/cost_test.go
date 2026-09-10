package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedHistory(t *testing.T) backfillPaths {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()
	sessDir := filepath.Join(dir, "session-runs", "abcd")
	writeJSON(t, filepath.Join(sessDir, "run1.json"), runhistory.SessionRun{RunID: "1111111111111111", SessionKey: "feishu:p2p:u", StartedAt: now.Add(-24 * time.Hour), CostUSD: 0.4})
	writeJSON(t, filepath.Join(sessDir, "run2.json"), runhistory.SessionRun{RunID: "2222222222222222", SessionKey: "feishu:p2p:u", StartedAt: now.Add(-2 * time.Hour), CostUSD: 0})
	writeJSON(t, filepath.Join(sessDir, "old.json"), runhistory.SessionRun{RunID: "3333333333333333", SessionKey: "feishu:p2p:u", StartedAt: now.Add(-500 * 24 * time.Hour), CostUSD: 9})
	os.WriteFile(filepath.Join(sessDir, "bad.json"), []byte("{"), 0o600)
	writeJSON(t, filepath.Join(sessDir, "cronsess.json"), runhistory.SessionRun{RunID: "4444444444444444", SessionKey: "cron:0123456789abcdef", StartedAt: now.Add(-3 * time.Hour), CostUSD: 0.2})
	cronDir := filepath.Join(dir, "cron", "runs", "0123456789abcdef")
	writeJSON(t, filepath.Join(cronDir, "a.json"), cron.CronRun{RunID: "aaaaaaaaaaaaaaaa", JobID: "0123456789abcdef", StartedAt: now.Add(-3 * time.Hour), CostUSD: 0.2, Fresh: true, WorkDir: "/w/proj"})
	writeJSON(t, filepath.Join(cronDir, "b.json"), cron.CronRun{RunID: "bbbbbbbbbbbbbbbb", JobID: "0123456789abcdef", StartedAt: now.Add(-4 * time.Hour), CostUSD: 0.5, Fresh: false})
	writeJSON(t, filepath.Join(cronDir, "c.json"), cron.CronRun{RunID: "cccccccccccccccc", JobID: "0123456789abcdef", StartedAt: now.Add(-5 * time.Hour), SandboxMeta: &cron.SandboxRunMeta{CostUSD: 1.5}})
	return backfillPaths{
		SessionStorePath: filepath.Join(dir, "sessions.json"),
		CronStorePath:    filepath.Join(dir, "cron", "cron_jobs.json"),
	}
}

// TestBackfill_CronRunsFollowTheCronStoreNotTheSessionStore pins the split that
// made three of datadir's original constructors unusable (#2641):
// `session.store_path` and `cron.store_path` are configured independently with
// no same-directory constraint, so cron's run records live beside CRON's store.
// A layout helper that rooted cron state under the session directory would find
// nothing here — and an operator's run history would quietly vanish.
//
// seedHistory already places the two stores in different directories, so this
// asserts the property directly rather than relying on the import count to
// notice.
func TestBackfill_CronRunsFollowTheCronStoreNotTheSessionStore(t *testing.T) {
	p := seedHistory(t)
	sessionRoot := datadir.ForStore(p.SessionStorePath)
	cronRoot := datadir.ForStore(p.CronStorePath)
	if sessionRoot.Root() == cronRoot.Root() {
		t.Fatal("fixture no longer splits the two store directories; this test would prove nothing")
	}
	if _, err := os.Stat(cronRoot.RunsRoot()); err != nil {
		t.Fatalf("cron runs must live under the cron store dir (%s): %v", cronRoot.RunsRoot(), err)
	}
	if _, err := os.Stat(sessionRoot.RunsRoot()); err == nil {
		t.Errorf("found a runs/ tree under the SESSION store dir (%s); cron state must not be rooted there", sessionRoot.RunsRoot())
	}

	var out bytes.Buffer
	rep, err := backfillLedger(p, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	// Both roots were read: the cron records only exist under the cron root, and
	// the session-runs records only under the session root.
	if rep.CronLocal == 0 || rep.CronSandbox == 0 {
		t.Errorf("cron records not found under %s: %+v\n%s", cronRoot.RunsRoot(), rep, out.String())
	}
	if rep.SessionRuns == 0 {
		t.Errorf("session-runs records not found under %s: %+v", sessionRoot.SessionRunsRoot(), rep)
	}
}

func TestBackfill_ImportsDedupsAndSkipsUnreliable(t *testing.T) {
	p := seedHistory(t)
	var out bytes.Buffer
	rep, err := backfillLedger(p, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Imported != 3 || rep.SkippedCumulative != 1 || rep.SkippedOld != 1 || rep.Unreadable != 1 || rep.SessionRuns != 4 {
		t.Fatalf("report = %+v\n%s", rep, out.String())
	}
	store := costledger.NewStore(filepath.Join(filepath.Dir(p.SessionStorePath), "cost"), costledger.Options{})
	defer store.Close()
	sum, err := store.Summarize(costledger.Query{GroupBy: costledger.GroupBySource, From: time.Now().Add(-48 * time.Hour), To: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, b := range sum.Buckets {
		got[b.Key] = b.Amount
	}
	if got["session"] != 0.4 || got["cron_local"] != 0.2 || got["cron_sandbox"] != 1.5 || sum.Kinds["backfill"] != 3 {
		t.Fatalf("buckets = %v kinds=%v", got, sum.Kinds)
	}
	store.Close()

	// Second run: everything is already there.
	rep2, err := backfillLedger(p, false, &out)
	if err != nil || rep2.Imported != 0 || rep2.Duplicate != 3 {
		t.Fatalf("second run = %+v err=%v", rep2, err)
	}
}

func TestBackfill_DryRunWritesNothing(t *testing.T) {
	p := seedHistory(t)
	rep, err := backfillLedger(p, true, &bytes.Buffer{})
	if err != nil || rep.Imported != 3 {
		t.Fatalf("dry run report = %+v err=%v", rep, err)
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(p.SessionStorePath), "cost", "*.jsonl"))
	if len(files) != 0 {
		t.Fatalf("dry run must not write day files: %v", files)
	}
}

func TestBackfill_RefusesWithoutStoreOrWhenDisabled(t *testing.T) {
	if _, err := backfillLedger(backfillPaths{}, true, &bytes.Buffer{}); err == nil {
		t.Fatal("empty store path must error")
	}
	off := false
	p := seedHistory(t)
	p.Cost = config.CostConfig{Enabled: &off}
	if _, err := backfillLedger(p, true, &bytes.Buffer{}); err == nil {
		t.Fatal("disabled ledger must error")
	}
}
