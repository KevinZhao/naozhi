package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestLinker_Resolve_HoistedSlicesRetry pins [R112714-PERF-3]: candidates and
// filtered slices are reused across retry attempts. This test exercises a
// two-attempt scenario where the first scan finds nothing (empty dir), then
// after a brief wait the agent file appears and the second scan succeeds.
// Correct pick proves the hoisted [:0] reuse doesn't accumulate stale state.
func TestLinker_Resolve_HoistedSlicesRetry(t *testing.T) {
	t.Parallel()
	const sessionID = "f0f0f0f0-1234-5678-90ab-cdef01234567"
	l, subagentDir := newLinkerForTest(t, sessionID)
	l.retryInterval = 20 * time.Millisecond
	l.retryLimit = 10

	const hex = "aabbccddeeff00112"
	const agentType = "retry-worker"
	now := time.Now()

	// Write the files just before Resolve starts so the first scan (within
	// the 200ms cache TTL window) may miss; the second scan will hit after
	// TTL expires. Using a scan hook to track when the dir is first scanned
	// and then writing the file forces the retry path.
	var scanCount int
	l.scanHook = func() {
		scanCount++
		if scanCount == 1 {
			// Write files after the first scan so the retry path is exercised.
			writeAgentFiles(t, subagentDir, hex, agentType, sessionID, "p-retry", now)
		}
	}
	l.cacheTTL = 1 * time.Millisecond // expire immediately so every retry rescans

	toolUseTime := now.Add(-50 * time.Millisecond).UnixMilli()
	info, resolved := l.Resolve(context.Background(), "t_retry", "toolu_R", agentType, "", toolUseTime)
	if !resolved {
		t.Fatalf("expected Resolve to succeed after retry, info=%+v", info)
	}
	want := filepath.Join(subagentDir, "agent-"+hex+".jsonl")
	if info.JSONLPath != want {
		t.Errorf("JSONLPath=%q, want %q", info.JSONLPath, want)
	}
	if info.FirstPromptID != "p-retry" {
		t.Errorf("FirstPromptID=%q, want p-retry", info.FirstPromptID)
	}
}

// TestLinker_Resolve_FirstLineMetaCachedAcrossRetries pins
// R20260607-PERF-2 (#1883): the retry loop must parse a stable candidate's
// first jsonl line at most once, reusing the cached firstLineMeta on every
// subsequent attempt instead of re-open+32KB-bufio+Unmarshal'ing it.
//
// Scenario: a candidate matches the requested agentType (so it survives into
// `candidates`) but its sessionId belongs to another session, so it is
// filtered out on every attempt and `filtered` stays empty — driving the loop
// through all retryLimit+1 attempts. Without the cache each attempt re-reads
// the same file; with it, readMetaHook fires exactly once.
func TestLinker_Resolve_FirstLineMetaCachedAcrossRetries(t *testing.T) {
	t.Parallel()
	const realSession = "cafebabe-1111-2222-3333-444444444444"
	const otherSession = "99998888-7777-6666-5555-444433332222"
	l, subagentDir := newLinkerForTest(t, realSession)
	l.retryInterval = 1 * time.Millisecond
	l.retryLimit = 6
	l.cacheTTL = 1 * time.Millisecond // expire dir cache so each attempt rescans

	// Stable candidate: agentType matches but sessionId mismatches → always
	// filtered out, so the loop runs every attempt against the same file.
	writeAgentFiles(t, subagentDir, "feedfacecafe00011", "cache-worker", otherSession, "p_x", time.Now())

	var reads int
	l.readMetaHook = func() { reads++ }

	toolUseTime := time.Now().UnixMilli()
	info, resolved := l.Resolve(context.Background(), "t_cache", "toolu_K", "cache-worker", "", toolUseTime)
	// Mismatched sessionId → no real link (tombstone or unresolved).
	if resolved && info.InternalAgentID != "" {
		t.Fatalf("mismatched-session candidate should not resolve to a real agent, got %+v", info)
	}
	if reads != 1 {
		t.Errorf("readFirstLineMeta cache misses = %d across %d attempts, want 1 (stable first line must be parsed once)", reads, l.retryLimit+1)
	}
}

// TestLinker_Resolve_HoistedSlicesMultiCandidate verifies that hoisted slices
// don't accumulate stale entries when multiple candidates exist and the best
// one is selected by mtime.
func TestLinker_Resolve_HoistedSlicesMultiCandidate(t *testing.T) {
	t.Parallel()
	const sessionID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	l, subagentDir := newLinkerForTest(t, sessionID)

	oldTime := time.Now().Add(-2 * time.Hour)
	newTime := time.Now()

	writeAgentFiles(t, subagentDir, "0000000000000001a", "multi-worker", sessionID, "p_old", oldTime)
	writeAgentFiles(t, subagentDir, "0000000000000002b", "multi-worker", sessionID, "p_new", newTime)

	// Add a file with a different agentType that should NOT appear in candidates.
	writeAgentFiles(t, subagentDir, "0000000000000003c", "other-type", sessionID, "p_other", newTime)

	toolUseTime := newTime.Add(-50 * time.Millisecond).UnixMilli()
	info, resolved := l.Resolve(context.Background(), "t_multi", "toolu_M", "multi-worker", "", toolUseTime)
	if !resolved {
		t.Fatalf("Resolve failed")
	}
	if info.InternalAgentID != "agent-0000000000000002b" {
		t.Errorf("expected newest candidate, got InternalAgentID=%q", info.InternalAgentID)
	}
	// Verify the other-type candidate was NOT picked.
	if info.JSONLPath == filepath.Join(subagentDir, "agent-0000000000000003c.jsonl") {
		t.Error("other-type candidate leaked into filtered slice")
	}
}
