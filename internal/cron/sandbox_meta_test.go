package cron

import (
	"encoding/json"
	"path/filepath"
	"reflect"
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

	s.executeOpt(j.ID, true)
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
	if !reflect.DeepEqual(got, want) {
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

// TestSandboxRunMeta_ZeroOmitsAllKeys: a zero receipt must serialise to {}
// (every field omitempty) so sandboxMetaPtr's IsZero gate is the only thing
// deciding attachment, never a half-empty key set.
func TestSandboxRunMeta_ZeroOmitsAllKeys(t *testing.T) {
	if !(SandboxRunMeta{}).IsZero() {
		t.Fatal("zero SandboxRunMeta must report IsZero")
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
