package cron

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Both loaders below read through osutil/jsonfile, which bounds the read and
// refuses a symlink. jsonfile reports three outcomes, and these tests pin the
// mapping each caller needs — the interesting half is that "corrupt" must NOT
// collapse into "absent", on the first read or any later one.

// TestGetSandboxAttention_CorruptFailsClosedOnEveryRead: the attention record
// is what proves the original sandbox run was stopped, so an unreadable one
// has to be an error — and it has to stay one. Moving the file aside on the
// first read would make the second read "no record", and a retried replay
// would dispatch against a run that may still be live
// (TestReplay_CorruptAttentionFailsClosed guards that end to end).
func TestGetSandboxAttention_CorruptFailsClosedOnEveryRead(t *testing.T) {
	t.Parallel()
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, filepath.Join(t.TempDir(), "cron_jobs.json"))
	dir := s.sandboxAttentionDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runID := mustGenerateRunID()
	path := filepath.Join(dir, runID+".json")
	if err := os.WriteFile(path, []byte("{not valid"), 0o600); err != nil {
		t.Fatal(err)
	}

	for read := 1; read <= 2; read++ {
		rec, ok, err := s.getSandboxAttention(runID)
		if !errors.Is(err, errCorruptAttentionRecord) {
			t.Fatalf("read %d = (%v, %v, %v), want errCorruptAttentionRecord", read, rec, ok, err)
		}
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("the unparseable record left its path (%v); it must stay until an operator removes it", serr)
	}
}

// TestGetSandboxAttention_AbsentIsNotAnError keeps the other half of the
// mapping: a run with no attention record is the normal case.
func TestGetSandboxAttention_AbsentIsNotAnError(t *testing.T) {
	t.Parallel()
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, filepath.Join(t.TempDir(), "cron_jobs.json"))
	rec, ok, err := s.getSandboxAttention(mustGenerateRunID())
	if err != nil || ok || rec != nil {
		t.Errorf("absent record = (%v, %v, %v), want (nil, false, nil)", rec, ok, err)
	}
}

// TestGetSandboxAttention_OversizeIsRefused: the record carries a handful of
// ids and a label, so a multi-megabyte one is a tampered or runaway file and
// must not be allocated.
func TestGetSandboxAttention_OversizeIsRefused(t *testing.T) {
	t.Parallel()
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, filepath.Join(t.TempDir(), "cron_jobs.json"))
	dir := s.sandboxAttentionDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runID := mustGenerateRunID()
	// Valid JSON, just far too big: padding rides in an unknown field, which
	// encoding/json ignores. A payload that is merely malformed would be
	// refused as corrupt even with no cap at all, so it could not tell whether
	// the cap is doing anything.
	pad := make([]byte, maxAttentionRecordBytes)
	for i := range pad {
		pad[i] = 'x'
	}
	body := `{"run_id":"` + runID + `","job_id":"0123456789abcdef","pad":"` + string(pad) + `"}`
	if err := os.WriteFile(filepath.Join(dir, runID+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := s.getSandboxAttention(runID); err == nil || ok {
		t.Error("an over-cap record must be refused rather than parsed")
	}
}

// TestSandboxRunSnapshotManifest_CorruptIsUnreadableNotMissing: the read that
// finds a corrupt manifest reports it as unreadable rather than as a run that
// never had one.
func TestSandboxRunSnapshotManifest_CorruptIsUnreadableNotMissing(t *testing.T) {
	t.Parallel()
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, filepath.Join(t.TempDir(), "cron_jobs.json"))
	jobID, runID := mustGenerateID(), mustGenerateRunID()
	dir := filepath.Join(s.sandboxSnapshotDir(), jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runID+".json"), []byte("{truncated"), 0o600); err != nil {
		t.Fatal(err)
	}

	man, ok, err := s.SandboxRunSnapshotManifest(jobID, runID)
	if err == nil || ok || man != nil {
		t.Errorf("corrupt manifest = (%v, %v, %v), want an error", man, ok, err)
	}
}

// TestListSandboxAttention_ListsUnreadableRecords: a record that cannot be read
// still blocks replay of its run, so the queue has to show it or the operator
// has no way to clear it. It is listed under its file-name run id with no job,
// next to the readable records rather than instead of them; a file whose name
// is not a run id could not be confirmed through the API and stays unlisted.
func TestListSandboxAttention_ListsUnreadableRecords(t *testing.T) {
	t.Parallel()
	s, _ := sandboxTestScheduler(t, &fakeSandboxRunner{}, filepath.Join(t.TempDir(), "cron_jobs.json"))
	goodJob, goodRun := mustGenerateID(), mustGenerateRunID()
	s.WriteSandboxAttentionForTest(goodJob, goodRun, attentionReasonTransport, "nightly")
	dir := s.sandboxAttentionDir()
	badRun := mustGenerateRunID()
	if err := os.WriteFile(filepath.Join(dir, badRun+".json"), []byte("{not valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-an-id.json"), []byte("{not valid"), 0o600); err != nil {
		t.Fatal(err)
	}

	items := s.ListSandboxAttention()
	byRun := map[string]SandboxAttentionItem{}
	for _, it := range items {
		byRun[it.RunID] = it
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v, want the readable record and the unreadable one", items)
	}
	if got := byRun[goodRun]; got.Unreadable || got.JobID != goodJob {
		t.Errorf("readable record = %+v", got)
	}
	bad, ok := byRun[badRun]
	if !ok || !bad.Unreadable || bad.Reason != attentionReasonUnreadable || bad.JobID != "" {
		t.Fatalf("unreadable record = %+v (listed %v), want Unreadable under its file-name run id with no job", bad, ok)
	}
	if bad.CreatedAtMS <= 0 {
		t.Error("an unreadable item carries no queue time; want the file's mtime")
	}

	// Confirming it clears it, and nothing is left to block replay.
	if err := s.ConfirmSandboxRun(badRun); err != nil {
		t.Fatalf("ConfirmSandboxRun: %v", err)
	}
	if _, ok, err := s.getSandboxAttention(badRun); ok || err != nil {
		t.Errorf("after confirm the record reads (%v, %v), want absent", ok, err)
	}
}
