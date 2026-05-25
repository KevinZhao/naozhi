package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests pin RFC v4 agent-team-ui §3.3 SubagentLinker behaviour:
//   1. ResolveProjectDir encoding (R7, §3.3.2) — "[^A-Za-z0-9]→'-'" no collapse.
//   2. Resolve 7-step algorithm (§3.3.1) — guard, scan, filter by agentType,
//      retry on empty, mtime-desc selector, sessionId cross-check, cache write.
//   3. dirCache 200ms TTL (§3.3.3) — same-turn bursts share a scan, retry
//      windows miss and rescan.
//   4. SeedFromHistory (§3.3.7) — reconnect path bypasses scan.
//   5. Same-name respawn defence (§3.3.1 step 7b) — FirstPromptID divergence
//      triggers warn, original mapping retained.
//   6. Tombstone behaviour after grace timeout.
//   7. OnResolve callback fires with (taskID, toolUseID, internalAgentID).

func TestResolveProjectDir(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/home/ec2-user/.claude":          "-home-ec2-user--claude",
		"/home/ec2-user/workspace/naozhi": "-home-ec2-user-workspace-naozhi",
		"/tmp/a--b":                       "-tmp-a--b",
		"/tmp/a_b.c":                      "-tmp-a-b-c",
		"/tmp/spa ce":                     "-tmp-spa-ce",
		"/tmp/a-b":                        "-tmp-a-b",
		"":                                "",
		"/tmp/Abc123":                     "-tmp-Abc123",
		"/":                               "-",
		"/tmp/中文":                         "-tmp---", // 3-byte runes → 3 dashes each (CLI spec: non-alnum byte → '-')
	}
	// NOTE: the "chinese chars → 3 dashes each" case is byte-level because the
	// CLI in practice walks runes (range). The actual claude CLI walks runes,
	// so our resolveProjectDir does too. Expected output below reflects
	// "one rune → one dash" (not one byte).
	cases["/tmp/中文"] = "-tmp---"
	for cwd, want := range cases {
		got := resolveProjectDir(cwd)
		if cwd == "" {
			if got != "" {
				t.Errorf("cwd=%q got %q, want empty", cwd, got)
			}
			continue
		}
		wantFull := filepath.Join(os.Getenv("HOME"), ".claude", "projects", want)
		if got != wantFull {
			t.Errorf("cwd=%q got %q, want %q", cwd, got, wantFull)
		}
	}
}

// writeAgentFiles creates meta.json + jsonl pair for a test subagent.
// firstLine carries promptId/sessionId/timestamp used by Resolve step 5.
func writeAgentFiles(t *testing.T, dir, hex, agentType, sessionID, promptID string, ts time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	meta := map[string]any{"agentType": agentType}
	mb, _ := json.Marshal(meta)
	metaPath := filepath.Join(dir, "agent-"+hex+".meta.json")
	if err := os.WriteFile(metaPath, mb, 0o644); err != nil {
		t.Fatalf("meta: %v", err)
	}

	first := map[string]any{
		"parentUuid":  "p-root",
		"isSidechain": true,
		"agentId":     hex,
		"promptId":    promptID,
		"type":        "user",
		"sessionId":   sessionID,
		"timestamp":   ts.UTC().Format(time.RFC3339Nano),
	}
	fb, _ := json.Marshal(first)
	jsonlPath := filepath.Join(dir, "agent-"+hex+".jsonl")
	if err := os.WriteFile(jsonlPath, append(fb, '\n'), 0o644); err != nil {
		t.Fatalf("jsonl: %v", err)
	}
	// Force mtime so tests control ordering deterministically.
	if err := os.Chtimes(jsonlPath, ts, ts); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func newLinkerForTest(t *testing.T, sessionID string) (*SubagentLinker, string) {
	t.Helper()
	root := t.TempDir()
	l := NewSubagentLinker()
	projectDir := filepath.Join(root, "-project")
	subagentDir := filepath.Join(projectDir, sessionID, "subagents")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		t.Fatalf("mkdir subagentDir: %v", err)
	}
	l.SetContext(projectDir, sessionID)
	// Shrink defaults so the test suite does not sleep for real-world grace.
	l.retryInterval = 10 * time.Millisecond
	l.retryLimit = 5
	l.cacheTTL = 5 * time.Millisecond
	return l, subagentDir
}

func TestLinker_Resolve_SingleCandidate(t *testing.T) {
	t.Parallel()
	const sessionID = "11111111-2222-3333-4444-555555555555"
	l, subagentDir := newLinkerForTest(t, sessionID)
	now := time.Now()
	writeAgentFiles(t, subagentDir, "0123456789abcdef0", "lister-1", sessionID, "p1", now)

	toolUseTime := now.Add(-50 * time.Millisecond).UnixMilli()
	info, resolved := l.Resolve("t_aaa", "toolu_A", "lister-1", "", toolUseTime)
	if !resolved {
		t.Fatalf("expected Resolve to succeed, info=%+v", info)
	}
	if info.InternalAgentID != "agent-0123456789abcdef0" {
		t.Errorf("InternalAgentID=%q", info.InternalAgentID)
	}
	want := filepath.Join(subagentDir, "agent-0123456789abcdef0.jsonl")
	if info.JSONLPath != want {
		t.Errorf("JSONLPath=%q, want %q", info.JSONLPath, want)
	}
	if info.FirstPromptID != "p1" {
		t.Errorf("FirstPromptID=%q", info.FirstPromptID)
	}
	if !info.Resolved {
		t.Errorf("Resolved=false")
	}
}

func TestLinker_Resolve_MTimeDescSelector(t *testing.T) {
	t.Parallel()
	const sessionID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	l, subagentDir := newLinkerForTest(t, sessionID)

	oldTime := time.Now().Add(-1 * time.Hour)
	newTime := time.Now()

	// Two candidates with agentType=="worker" — same-turn CLI double-write
	// scenario (§1.4). Must pick the newer mtime ("complete version").
	writeAgentFiles(t, subagentDir, "aaaaaaaaaaaaaaaaa", "worker", sessionID, "p_old", oldTime)
	writeAgentFiles(t, subagentDir, "bbbbbbbbbbbbbbbbb", "worker", sessionID, "p_new", newTime)

	toolUseTime := newTime.Add(-50 * time.Millisecond).UnixMilli()
	info, resolved := l.Resolve("t_worker", "toolu_W", "worker", "", toolUseTime)
	if !resolved {
		t.Fatalf("Resolve failed")
	}
	if info.InternalAgentID != "agent-bbbbbbbbbbbbbbbbb" {
		t.Errorf("picked older candidate, InternalAgentID=%q", info.InternalAgentID)
	}
	if info.FirstPromptID != "p_new" {
		t.Errorf("FirstPromptID=%q, want p_new", info.FirstPromptID)
	}
}

func TestLinker_Resolve_SessionIDMismatch_Skip(t *testing.T) {
	t.Parallel()
	const realSession = "deadbeef-1111-2222-3333-444444444444"
	const otherSession = "11112222-3333-4444-5555-666677778888"
	l, subagentDir := newLinkerForTest(t, realSession)

	// Cross-contamination scenario (R7): projectDir collision across cwds
	// leaves a stray jsonl from another session. Must skip.
	writeAgentFiles(t, subagentDir, "11111111111111111", "collider", otherSession, "p_other", time.Now())

	toolUseTime := time.Now().UnixMilli()
	info, resolved := l.Resolve("t_coll", "toolu_C", "collider", "", toolUseTime)
	if resolved && info.InternalAgentID != "" {
		t.Errorf("cross-session jsonl leaked: %+v", info)
	}
}

func TestLinker_Resolve_StaleCandidate_Skip(t *testing.T) {
	t.Parallel()
	const sessionID = "cafecafe-1111-2222-3333-444444444444"
	l, subagentDir := newLinkerForTest(t, sessionID)

	// jsonl timestamp > 10s OLDER than the Agent tool_use — stale candidate
	// (R10 §3.3.1 step 5). Claude CLI should never emit this, but be strict.
	fileTime := time.Now().Add(-1 * time.Minute)
	writeAgentFiles(t, subagentDir, "22222222222222222", "stale", sessionID, "p_stale", fileTime)

	toolUseTime := time.Now().UnixMilli()
	info, resolved := l.Resolve("t_stale", "toolu_S", "stale", "", toolUseTime)
	if resolved && info.InternalAgentID != "" {
		t.Errorf("stale candidate accepted: %+v", info)
	}
}

func TestLinker_Resolve_RetryThenSucceed(t *testing.T) {
	t.Parallel()
	const sessionID = "11111111-2222-3333-4444-aaaaaaaaaaaa"
	l, subagentDir := newLinkerForTest(t, sessionID)
	l.retryInterval = 15 * time.Millisecond
	l.retryLimit = 10

	// Fire Resolve first; have file appear mid-grace. Simulates "CLI writes
	// meta.json 30ms after task_started".
	toolUseTime := time.Now().UnixMilli()
	// defer-join the writer goroutine: without it a late wakeup races t.TempDir
	// cleanup and the goroutine's t.Fatalf fires after the test is done.
	done := make(chan struct{})
	defer func() { <-done }()
	go func() {
		defer close(done)
		time.Sleep(30 * time.Millisecond)
		writeAgentFiles(t, subagentDir, "33333333333333333", "delayed", sessionID, "p_d", time.Now())
	}()
	info, resolved := l.Resolve("t_d", "toolu_D", "delayed", "", toolUseTime)
	if !resolved {
		t.Fatalf("Resolve should succeed after retry, info=%+v", info)
	}
	if info.InternalAgentID != "agent-33333333333333333" {
		t.Errorf("InternalAgentID=%q", info.InternalAgentID)
	}
}

func TestLinker_Resolve_Timeout_Tombstone(t *testing.T) {
	t.Parallel()
	const sessionID = "11111111-2222-3333-4444-bbbbbbbbbbbb"
	l, subagentDir := newLinkerForTest(t, sessionID)
	_ = subagentDir
	l.retryInterval = 5 * time.Millisecond
	l.retryLimit = 3

	toolUseTime := time.Now().UnixMilli()
	info, resolved := l.Resolve("t_miss", "toolu_M", "missing", "", toolUseTime)
	if !resolved {
		t.Fatalf("tombstone should be 'Resolved' (§3.3.1 step 4), info=%+v", info)
	}
	if info.InternalAgentID != "" {
		t.Errorf("tombstone InternalAgentID should be empty, got %q", info.InternalAgentID)
	}

	// Second call returns cached tombstone without scanning again.
	info2, resolved2 := l.Resolve("t_miss", "toolu_M", "missing", "", toolUseTime)
	if !resolved2 || info2.InternalAgentID != "" {
		t.Errorf("cached tombstone missing: info2=%+v resolved=%v", info2, resolved2)
	}
}

func TestLinker_Resolve_DirCacheTTL(t *testing.T) {
	t.Parallel()
	const sessionID = "11111111-2222-3333-4444-cccccccccccc"
	l, subagentDir := newLinkerForTest(t, sessionID)
	l.cacheTTL = 50 * time.Millisecond

	// Install a counting scan hook to observe cache hits vs misses.
	var scans atomic.Int32
	l.scanHook = func() { scans.Add(1) }

	now := time.Now()
	writeAgentFiles(t, subagentDir, "44444444444444444", "a1", sessionID, "p1", now)
	writeAgentFiles(t, subagentDir, "55555555555555555", "a2", sessionID, "p2", now)

	toolUseTime := now.Add(-10 * time.Millisecond).UnixMilli()
	// Two Resolves within the TTL window — share ONE scan.
	if _, ok := l.Resolve("t_a1", "toolu_A1", "a1", "", toolUseTime); !ok {
		t.Fatalf("Resolve a1 failed")
	}
	if _, ok := l.Resolve("t_a2", "toolu_A2", "a2", "", toolUseTime); !ok {
		t.Fatalf("Resolve a2 failed")
	}
	if got := scans.Load(); got != 1 {
		t.Errorf("scans within TTL = %d, want 1", got)
	}

	// Wait past TTL, write a third and Resolve — expect a fresh scan.
	time.Sleep(70 * time.Millisecond)
	writeAgentFiles(t, subagentDir, "66666666666666666", "a3", sessionID, "p3", time.Now())
	if _, ok := l.Resolve("t_a3", "toolu_A3", "a3", "", time.Now().UnixMilli()); !ok {
		t.Fatalf("Resolve a3 failed")
	}
	if got := scans.Load(); got != 2 {
		t.Errorf("post-TTL scans = %d, want 2", got)
	}
}

func TestLinker_SeedFromHistory_Bypasses_Scan(t *testing.T) {
	// Not Parallel — t.Setenv redirects HOME for this test only.
	home := t.TempDir()
	t.Setenv("HOME", home)

	l := NewSubagentLinker()
	var scans atomic.Int32
	l.scanHook = func() { scans.Add(1) }

	// Simulate historical EventEntry list (what persistHistory would replay
	// on shim reconnect). SeedFromHistory must NOT touch disk. The path
	// must live under ~/.claude/projects because R201-SEC-M1's prefix
	// check now rejects anything outside that root even for seeded entries.
	jsonl := filepath.Join(home, ".claude", "projects", "-test", "s", "subagents", "agent-77777777777777777.jsonl")
	entries := []EventEntry{
		{Type: "agent", ToolUseID: "toolu_H", Subagent: "hist-1"},
		{
			Type:            "task_start",
			ToolUseID:       "toolu_H",
			TaskID:          "t_hist",
			InternalAgentID: "agent-77777777777777777",
			JSONLPath:       jsonl,
			FirstPromptID:   "p_hist",
			Subagent:        "hist-1",
		},
	}
	l.SeedFromHistory(entries)

	info, ok := l.Query("t_hist")
	if !ok {
		t.Fatalf("Query after seed failed")
	}
	if info.InternalAgentID != "agent-77777777777777777" {
		t.Errorf("seeded InternalAgentID=%q", info.InternalAgentID)
	}
	if !info.FromHistory {
		t.Errorf("FromHistory=false")
	}
	if scans.Load() != 0 {
		t.Errorf("SeedFromHistory touched disk: scans=%d", scans.Load())
	}
}

func TestLinker_SameName_PromptIDDivergence_Keeps_Original(t *testing.T) {
	t.Parallel()
	const sessionID = "11111111-2222-3333-4444-dddddddddddd"
	l, subagentDir := newLinkerForTest(t, sessionID)

	now := time.Now()
	writeAgentFiles(t, subagentDir, "88888888888888888", "dup", sessionID, "p_first", now.Add(-5*time.Second))
	// First resolve binds toolu_A → agent-8888... with p_first
	info1, ok := l.Resolve("t1", "toolu_A", "dup", "", now.Add(-4*time.Second).UnixMilli())
	if !ok || info1.FirstPromptID != "p_first" {
		t.Fatalf("first resolve: %+v", info1)
	}

	// Simulate same-name re-spawn — CLI writes a SECOND jsonl with different
	// promptId and later mtime.
	writeAgentFiles(t, subagentDir, "99999999999999999", "dup", sessionID, "p_second", now)
	// Expire dirCache so the second Resolve actually sees the new file.
	time.Sleep(l.cacheTTL + 5*time.Millisecond)
	info2, ok := l.Resolve("t2", "toolu_B", "dup", "", now.Add(-100*time.Millisecond).UnixMilli())
	if !ok {
		t.Fatalf("second resolve failed")
	}
	if info2.FirstPromptID != "p_second" {
		t.Errorf("second resolve FirstPromptID=%q, want p_second", info2.FirstPromptID)
	}

	// Original task_id must still map to the ORIGINAL hex (step 7b: warn + keep old).
	originalInfo, ok := l.Query("t1")
	if !ok || originalInfo.InternalAgentID != "agent-88888888888888888" {
		t.Errorf("original mapping overwritten: %+v", originalInfo)
	}
}

func TestLinker_OnResolve_Fires(t *testing.T) {
	t.Parallel()
	const sessionID = "11111111-2222-3333-4444-eeeeeeeeeeee"
	l, subagentDir := newLinkerForTest(t, sessionID)

	type cbArg struct{ taskID, toolUseID, hex string }
	ch := make(chan cbArg, 4)
	l.OnResolve(func(taskID, toolUseID, hex string) {
		ch <- cbArg{taskID, toolUseID, hex}
	})

	now := time.Now()
	writeAgentFiles(t, subagentDir, "abcabcabcabcabcab", "solo", sessionID, "p_s", now)

	toolUseTime := now.Add(-20 * time.Millisecond).UnixMilli()
	if _, ok := l.Resolve("t_solo", "toolu_S", "solo", "", toolUseTime); !ok {
		t.Fatal("Resolve failed")
	}
	select {
	case arg := <-ch:
		if arg.taskID != "t_solo" || arg.toolUseID != "toolu_S" || arg.hex != "agent-abcabcabcabcabcab" {
			t.Errorf("callback args = %+v", arg)
		}
	case <-time.After(time.Second):
		t.Fatal("onResolve callback never fired")
	}
}

func TestLinker_PreContext_Resolve_NoOp(t *testing.T) {
	t.Parallel()
	l := NewSubagentLinker()
	// SetContext not called — Resolve must bail out, not scan.
	info, resolved := l.Resolve("t", "u", "x", "", time.Now().UnixMilli())
	if resolved || info.InternalAgentID != "" {
		t.Errorf("pre-context Resolve returned %+v resolved=%v", info, resolved)
	}
}

func TestLinker_Query_DefaultEmpty(t *testing.T) {
	t.Parallel()
	l := NewSubagentLinker()
	info, ok := l.Query("unknown")
	if ok {
		t.Errorf("unknown task_id: info=%+v ok=true", info)
	}
	_ = strings.Split // keep import if needed later
}

// TestReadFirstLineMeta_OversizedFirstLine pins R222-GO-7: when the first
// line of an agent jsonl exceeds the 32KB ReadSlice buffer, readFirstLineMeta
// must surface errFirstLineTooLong instead of silently passing a truncated
// prefix to json.Unmarshal (which previously yielded a generic JSON syntax
// error that callers logged as a misleading "fast-path stat miss").
func TestReadFirstLineMeta_OversizedFirstLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-deadbeef.jsonl")
	// Build a single line >32KB with no '\n' until the very end.
	pad := strings.Repeat("x", 64*1024)
	content := []byte(`{"sessionId":"abc","promptId":"p1","extra":"` + pad + `"}` + "\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readFirstLineMeta(path); err != errFirstLineTooLong {
		t.Errorf("got err=%v, want errFirstLineTooLong", err)
	}
}
