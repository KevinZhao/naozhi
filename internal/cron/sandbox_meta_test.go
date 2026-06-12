package cron

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandbox_MetaFlowsIntoRunRecord pins PR-1: the cloud-execution receipt
// (cost / memory / image / exit / runtime arn) the adapter fills into
// SandboxOutcome.Meta must reach the persisted CronRun.SandboxMeta — this
// is the data every §7.3/§7.5 dashboard deliverable renders.
func TestSandbox_MetaFlowsIntoRunRecord(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &fakeSandboxRunner{
		outcome: SandboxOutcome{
			State:      SandboxStateSuccess,
			ResultText: "done",
			Meta: SandboxRunMeta{
				RuntimeARN:      "arn:aws:bedrock-agentcore:us-west-2:1:runtime/x",
				ImageVersion:    "phase2",
				ExitStatus:      0,
				CostUSD:         0.0123,
				DurationMS:      4567,
				MemoryPeakBytes: 268435456,
			},
		},
	}
	s, rec := sandboxTestScheduler(t, runner, storePath)
	j := sandboxJob(t, s)

	s.executeOpt(j, true)
	waitEnded(t, rec)

	run, err := s.Run(j.ID, rec.endedAtCron(0).RunID)
	if err != nil {
		t.Fatalf("read run record: %v", err)
	}
	if run.SandboxMeta == nil {
		t.Fatal("CronRun.SandboxMeta must be populated for a sandbox run")
	}
	got := *run.SandboxMeta
	want := runner.outcome.Meta
	if got != want {
		t.Fatalf("SandboxMeta = %+v, want %+v", got, want)
	}
}

// TestSandbox_MetaAbsentForLocalRuns: a local (placement="") run must
// persist NO sandbox_meta key — the field is wire-read-safe only because
// local records stay byte-identical to pre-Phase-2.
func TestSandbox_MetaAbsentForLocalRuns(t *testing.T) {
	r := &CronRun{RunID: "a", JobID: "b", State: RunStateSucceeded}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(data); strings.Contains(got, "sandbox_meta") {
		t.Fatalf("local run JSON must not carry sandbox_meta: %s", got)
	}
}

// TestSandbox_MetaExcludedFromSummary pins the list-endpoint payload guard:
// recent_runs loads 50 jobs × 5 summaries — receipts would bloat it. The
// summary() projection must drop SandboxMeta.
func TestSandbox_MetaExcludedFromSummary(t *testing.T) {
	r := &CronRun{
		RunID: "a", JobID: "b", State: RunStateSucceeded,
		SandboxMeta: &SandboxRunMeta{CostUSD: 1.23, ImageVersion: "phase2"},
	}
	data, err := json.Marshal(r.summary())
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if got := string(data); strings.Contains(got, "sandbox_meta") || strings.Contains(got, "phase2") || strings.Contains(got, "cost") {
		t.Fatalf("CronRunSummary must not carry sandbox meta: %s", got)
	}
}

// TestSandboxRunMeta_WireTags freezes the JSON tags — the dashboard run
// detail (§7.3) keys off these literals, so a rename must fail here first.
func TestSandboxRunMeta_WireTags(t *testing.T) {
	m := SandboxRunMeta{
		RuntimeARN:      "arn",
		ImageVersion:    "v1",
		ExitStatus:      2,
		CostUSD:         0.5,
		DurationMS:      10,
		MemoryPeakBytes: 99,
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"runtime_arn":`, `"image_version":`, `"exit_status":`,
		`"cost_usd":`, `"duration_ms":`, `"memory_peak_bytes":`,
	} {
		if !strings.Contains(string(data), key) {
			t.Errorf("SandboxRunMeta JSON missing wire key %s; got %s", key, data)
		}
	}
}

// TestSandboxRunMeta_ZeroOmitsAllKeys: a zero receipt must serialise to {}
// (every field omitempty) so sandboxMetaPtr's isZero gate is the only thing
// deciding attachment, never a half-empty key set.
func TestSandboxRunMeta_ZeroOmitsAllKeys(t *testing.T) {
	if !(SandboxRunMeta{}).isZero() {
		t.Fatal("zero SandboxRunMeta must report isZero")
	}
	// ExitStatus has no omitempty (exit 0 is meaningful), so a zero receipt
	// serialises to {"exit_status":0} — but the enclosing pointer is
	// omitempty, and sandboxMetaPtr(zero)==nil means it never reaches JSON.
	data, _ := json.Marshal(SandboxRunMeta{})
	if string(data) != `{"exit_status":0}` {
		t.Fatalf("zero SandboxRunMeta = %s, want {\"exit_status\":0}", data)
	}
	if sandboxMetaPtr(SandboxRunMeta{}) != nil {
		t.Fatal("sandboxMetaPtr(zero) must be nil so the record grows no key")
	}
	if sandboxMetaPtr(SandboxRunMeta{CostUSD: 0.01}) == nil {
		t.Fatal("sandboxMetaPtr(non-zero) must return a pointer")
	}
}
