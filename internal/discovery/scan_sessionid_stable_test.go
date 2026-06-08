//go:build linux

package discovery

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScan_SessionIDNeverUpgradedToOtherSessionJSONL is the regression guard
// for the discovered-CLI "crosstalk" bug: a single external CLI process that
// owns a CWD had its SessionID silently rewritten to the newest JSONL in that
// project directory, even when that JSONL belonged to a *different* session
// (a naozhi-managed session, a closed session, or a cron run — they all write
// into the same ~/.claude/projects/<cwd>/ directory).
//
// The result was the dashboard sidebar card (and its preview) showing another
// session's content under this process, and takeover --resume targeting the
// wrong conversation.
//
// Modern Claude CLI keeps {pid}.json's sessionId accurate, so the upgrade
// heuristic is now pure harm: the discovered SessionID MUST equal the value
// recorded in {pid}.json, never a sibling JSONL's id.
func TestScan_SessionIDNeverUpgradedToOtherSessionJSONL(t *testing.T) {
	resetCaches(t)
	claudeDir := makeClaudeDir(t)
	sessDir := filepath.Join(claudeDir, "sessions")

	pid := os.Getpid() // current process so processAlive() is true
	cwd := "/tmp/scan-sessionid-stable"

	// The process's real session, as recorded in {pid}.json.
	ownSession := "aaaaaaaa-1111-1111-1111-000000000001"
	makeSessionFile(t, sessDir, sessionFile{
		PID:        pid,
		SessionID:  ownSession,
		CWD:        cwd,
		StartedAt:  time.Now().Add(-1 * time.Hour).UnixMilli(),
		Kind:       "interactive",
		Entrypoint: "cli",
	})

	projDir := filepath.Join(claudeDir, "projects", projDirName(cwd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The process's own JSONL — present but older.
	ownJSONL := filepath.Join(projDir, ownSession+".jsonl")
	makeJSONLWithUserPrompts(t, ownJSONL, []string{"my own conversation"})
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(ownJSONL, old, old); err != nil {
		t.Fatal(err)
	}

	// A *different* session's JSONL in the same project dir, newer than ours.
	// Pre-upgrade this is exactly what got mis-assigned to the process.
	otherSession := "bbbbbbbb-2222-2222-2222-000000000002"
	otherJSONL := filepath.Join(projDir, otherSession+".jsonl")
	makeJSONLWithUserPrompts(t, otherJSONL, []string{"a different session's content"})
	// default mtime = now, i.e. newer than ownJSONL

	sessions, err := Scan(claudeDir, nil, nil, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	found := false
	for _, s := range sessions {
		if s.PID != pid {
			continue
		}
		found = true
		if s.SessionID != ownSession {
			t.Errorf("crosstalk: process SessionID = %q, want %q (the value from {pid}.json); "+
				"the upgrade heuristic mis-assigned a sibling JSONL", s.SessionID, ownSession)
		}
		if s.SessionID == otherSession {
			t.Errorf("crosstalk: process was assigned another session's JSONL id %q", otherSession)
		}
	}
	if !found {
		t.Fatalf("expected discovered session for pid %d; got %+v", pid, sessions)
	}
}

// TestScan_SessionIDStableWithNoOwnJSONLYet covers the freshly-/cleared
// process: {pid}.json points at a session whose JSONL has not been written
// yet (within the noJSONLGrace window), while an older unrelated JSONL sits in
// the same directory. The discovered SessionID must still be the {pid}.json
// value — we no longer "rescue" it by pointing at a stale sibling file.
func TestScan_SessionIDStableWithNoOwnJSONLYet(t *testing.T) {
	resetCaches(t)
	claudeDir := makeClaudeDir(t)
	sessDir := filepath.Join(claudeDir, "sessions")

	pid := os.Getpid()
	cwd := "/tmp/scan-sessionid-fresh"

	freshSession := "cccccccc-3333-3333-3333-000000000003"
	makeSessionFile(t, sessDir, sessionFile{
		PID:        pid,
		SessionID:  freshSession,
		CWD:        cwd,
		StartedAt:  time.Now().UnixMilli(), // just started → within noJSONLGrace
		Kind:       "interactive",
		Entrypoint: "cli",
	})

	projDir := filepath.Join(claudeDir, "projects", projDirName(cwd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// An unrelated, older JSONL sharing the directory.
	staleOther := "dddddddd-4444-4444-4444-000000000004"
	makeJSONLWithUserPrompts(t, filepath.Join(projDir, staleOther+".jsonl"),
		[]string{"unrelated older session"})

	sessions, err := Scan(claudeDir, nil, nil, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	found := false
	for _, s := range sessions {
		if s.PID != pid {
			continue
		}
		found = true
		if s.SessionID != freshSession {
			t.Errorf("SessionID = %q, want %q ({pid}.json value); must not adopt sibling %q",
				s.SessionID, freshSession, staleOther)
		}
	}
	if !found {
		t.Fatalf("expected fresh session for pid %d within noJSONLGrace; got %+v", pid, sessions)
	}
}
