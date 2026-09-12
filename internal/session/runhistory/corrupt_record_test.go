package runhistory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWarmMovesCorruptRecordAsideInsteadOfRereadingIt: warmLocked answers a
// readRunFile error with `continue`, so before jsonfile.Load an unparseable run
// record was skipped in place — re-read on every warm and never removed. The
// retention GC one line below the skip could not reach it either, so a corrupt
// file accumulated for the lifetime of the data dir. Now it is moved aside once.
func TestWarmMovesCorruptRecordAsideInsteadOfRereadingIt(t *testing.T) {
	root := t.TempDir()
	const key = "chat:abc"
	dir := filepath.Join(root, dirHashFor(key))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Both records are written straight to disk, then read by a store that has
	// never warmed this entry: Append marks the entry warmed, which would skip
	// warmLocked entirely and never reach readRunFile.
	// isValidRunID accepts lowercase hex only, so these names reach readRunFile.
	good := SessionRun{RunID: "aaaa0000deadbeef", SessionKey: key, StartedAt: time.Now()}
	body, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, good.RunID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bbbb0000deadbeef.json")
	if err := os.WriteFile(bad, []byte(`{"run_id": truncated`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := NewStore(root, 10, time.Hour).Recent(key, 0)
	if len(got) != 1 || got[0].RunID != good.RunID {
		t.Fatalf("Recent() = %+v, want just the parseable record", got)
	}
	if _, err := os.Stat(bad); err == nil {
		t.Error("corrupt record is still at its original path; it would be re-read on every warm")
	}
	sibs, globErr := filepath.Glob(bad + ".corrupt.*")
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(sibs) != 1 {
		t.Fatalf("corrupt siblings = %v, want exactly 1 (the bytes must survive for diagnosis)", sibs)
	}
}

// TestWarmRefusesOversizedRecord: without a cap, os.ReadFile slurped whatever
// was on disk. The record is left in place — an oversized file may be real data
// worth looking at — and simply not loaded.
func TestWarmRefusesOversizedRecord(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root, 10, time.Hour)
	const key = "chat:big"
	dir := filepath.Join(root, dirHashFor(key))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The oversized file must be VALID JSON that decodes into a usable
	// SessionRun, or the test would pass without the cap: json.Unmarshal would
	// reject garbage bytes anyway. A run whose session_id is padded past the cap
	// is exactly what an unbounded read used to slurp — SessionKey ignores
	// unknown padding, so only the cap can refuse this.
	huge := filepath.Join(dir, "cccc0000deadbeef.json")
	oversized, err := json.Marshal(SessionRun{
		RunID:      "cccc0000deadbeef",
		SessionKey: key,
		SessionID:  strings.Repeat("p", maxRunFileBytes),
		StartedAt:  time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(oversized)) <= maxRunFileBytes {
		t.Fatalf("fixture is %d bytes, not over the %d cap", len(oversized), maxRunFileBytes)
	}
	if err := os.WriteFile(huge, oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	// Sanity: the same bytes ARE loadable when the cap allows them, so the refusal
	// below is the cap's doing and not a malformed fixture.
	var probe SessionRun
	if err := json.Unmarshal(oversized, &probe); err != nil || probe.RunID != "cccc0000deadbeef" {
		t.Fatalf("fixture does not decode into a usable run: %v / %+v", err, probe)
	}
	if got := s.Recent(key, 0); len(got) != 0 {
		t.Fatalf("Recent() = %+v, want nothing loaded", got)
	}
	if _, err := os.Stat(huge); err != nil {
		t.Errorf("oversized record must be left in place for inspection: %v", err)
	}
}
