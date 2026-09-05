package sysession

import (
	"context"
	"expvar"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
)

const fakeResultJSON = `{"type":"result","subtype":"success","is_error":false,"result":"  标题：账本验收  ",` +
	`"total_cost_usd":0.0123,"modelUsage":{"us.anthropic.claude-fable-5-1[1m]":{"inputTokens":10,"outputTokens":5,` +
	`"cacheReadInputTokens":0,"cacheCreationInputTokens":200,"costUSD":0.0123,"canonicalModel":"claude-fable-5-1",` +
	`"provider":"bedrock","costBasis":"list"}}}`

// fakeRunner writes a shell script that prints stdout verbatim and returns a
// Runner bound to it plus a temp-dir ledger.
func fakeJSONRunner(t *testing.T, stdout string) (Runner, *costledger.Store) {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "sys-sessions")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-claude")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '" + strings.ReplaceAll(stdout, "'", `'"'"'`) + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := costledger.NewStore(filepath.Join(dir, "cost"), costledger.Options{})
	t.Cleanup(ledger.Close)
	r, err := NewRunner(RunnerConfig{BinPath: bin, WorkDir: work, Ledger: ledger})
	if err != nil {
		t.Fatal(err)
	}
	return r, ledger
}

func ledgerEntries(t *testing.T, l *costledger.Store) []costledger.Entry {
	t.Helper()
	l.Close()
	ents, err := l.Entries(costledger.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	return ents
}

func metricValue(t *testing.T, name string) int64 {
	t.Helper()
	v := expvar.Get(name)
	if v == nil {
		t.Fatalf("metric %s not registered", name)
	}
	n, err := strconv.ParseInt(v.String(), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRunner_Run_DecodesResultAndBooksCostToRun(t *testing.T) {
	r, ledger := fakeJSONRunner(t, fakeResultJSON)
	ctx := withRunInfo(context.Background(), "auto-titler", "0123456789abcdef")
	out, err := r.Run(ctx, "prompt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "标题：账本验收" {
		t.Fatalf("reply = %q, want the trimmed result string", out)
	}
	ents := ledgerEntries(t, ledger)
	if len(ents) != 1 {
		t.Fatalf("entries = %d: %+v", len(ents), ents)
	}
	e := ents[0]
	if e.Source != costledger.SourceSysession || e.Kind != costledger.KindTurn || e.SessionKey != "sys:auto-titler" ||
		e.RunID != "0123456789abcdef" || e.Backend != "claude" || e.Workspace != "sys-sessions" ||
		e.Unit != costledger.UnitUSD || e.Amount != 0.0123 || e.Basis != costledger.BasisList {
		t.Fatalf("entry = %+v", e)
	}
	if len(e.Models) != 1 || e.Models[0].Model != "claude-fable-5-1" || e.Models[0].CacheWrite != 200 {
		t.Fatalf("models = %+v", e.Models)
	}
}

func TestRunner_Run_OutsideManagedTickStillBooks(t *testing.T) {
	r, ledger := fakeJSONRunner(t, fakeResultJSON)
	if _, err := r.Run(context.Background(), "prompt"); err != nil {
		t.Fatal(err)
	}
	ents := ledgerEntries(t, ledger)
	if len(ents) != 1 || ents[0].SessionKey != "sys:unmanaged" || ents[0].RunID != "" {
		t.Fatalf("entries = %+v", ents)
	}
}

func TestRunner_Run_ErrorEnvelopeIsError(t *testing.T) {
	r, ledger := fakeJSONRunner(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom","total_cost_usd":0}`)
	_, err := r.Run(context.Background(), "prompt")
	if err == nil || !strings.Contains(err.Error(), "error_during_execution") {
		t.Fatalf("err = %v", err)
	}
	if ents := ledgerEntries(t, ledger); len(ents) != 0 {
		t.Fatalf("zero-cost error run must not book: %+v", ents)
	}
}

// A run that failed after spending money still books it (RFC: bill what was
// spent), while returning the error to the daemon.
func TestRunner_Run_ErrorEnvelopeWithSpendStillBooks(t *testing.T) {
	r, ledger := fakeJSONRunner(t, `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"gave up","total_cost_usd":0.05}`)
	_, err := r.Run(withRunInfo(context.Background(), "auto-titler", "00000000deadbeef"), "prompt")
	if err == nil || !strings.Contains(err.Error(), "error_max_turns") {
		t.Fatalf("err = %v", err)
	}
	ents := ledgerEntries(t, ledger)
	if len(ents) != 1 || ents[0].Amount != 0.05 || ents[0].RunID != "00000000deadbeef" {
		t.Fatalf("failed run spend must be booked: %+v", ents)
	}
}

func TestRunner_Run_NonJSONIsErrorNotGarbageReply(t *testing.T) {
	before := metricValue(t, "naozhi_sysession_runner_parse_fail_total")
	for _, out := range []string{"plain text title", `{"type":"result","result":"trunc`, `{"type":"assistant"}`} {
		r, _ := fakeJSONRunner(t, out)
		reply, err := r.Run(context.Background(), "prompt")
		if err == nil || reply != "" {
			t.Fatalf("stdout %q: reply=%q err=%v, want error and empty reply", out, reply, err)
		}
	}
	if got := metricValue(t, "naozhi_sysession_runner_parse_fail_total"); got != before+3 {
		t.Fatalf("parse-fail counter = %d, want %d", got, before+3)
	}
}

func TestRunInfoContext_RoundTrip(t *testing.T) {
	if _, ok := RunInfoFromContext(context.Background()); ok {
		t.Fatal("bare ctx must carry no run info")
	}
	ctx := withRunInfo(context.Background(), "d", "r")
	ri, ok := RunInfoFromContext(ctx)
	if !ok || ri.Daemon != "d" || ri.RunID != "r" {
		t.Fatalf("round trip = %+v %v", ri, ok)
	}
}

func TestRunnerStdoutCap_FitsJSONEnvelope(t *testing.T) {
	if runnerStdoutCapBytes < 256*1024 {
		t.Fatalf("cap %d too small for the json envelope around a 64 KiB reply", runnerStdoutCapBytes)
	}
}

// TestManager_TickContextCarriesRunInfo pins the attribution contract: the
// ctx a daemon receives in Tick identifies the run, so Runner calls inside
// Tick book their cost to the same run_id the DaemonRun record carries.
func TestManager_TickContextCarriesRunInfo(t *testing.T) {
	pulse, tickFn := pulseTicker()
	var seen RunInfo
	var seenOK bool
	d := &signalDaemon{name: "auto-titler", tickFn: func(ctx context.Context, _ int32) (TickReport, error) {
		seen, seenOK = RunInfoFromContext(ctx)
		return TickReport{Acted: 1}, nil
	}}
	withRegistry(t, []builtinDaemonFactory{
		{Name: "auto-titler", Build: func(deps DaemonDeps) (Daemon, error) { return d, nil }},
	})
	m, err := NewManager(Config{
		Enabled: true, TickTimeout: 100 * time.Millisecond, Router: newFakeRouter(),
		Daemons:   map[string]DaemonRuntimeConfig{"auto-titler": {Enabled: true, Tick: 50 * time.Millisecond}},
		NewTicker: tickFn,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())
	pulse <- time.Now()
	deadline := time.Now().Add(2 * time.Second)
	for d.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	var st []DaemonStatus
	for deadline = time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if st = m.Inspector(); len(st) > 0 && st[0].LastRun != nil {
			break
		}
	}
	if !seenOK || seen.Daemon != "auto-titler" || seen.RunID == "" {
		t.Fatalf("Tick ctx run info = %+v ok=%v", seen, seenOK)
	}
	if len(st) != 1 || st[0].LastRun == nil || st[0].LastRun.RunID != seen.RunID {
		t.Fatalf("run id on ctx %q must match the recorded run %+v", seen.RunID, st)
	}
}
