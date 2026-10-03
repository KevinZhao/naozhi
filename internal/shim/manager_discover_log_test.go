//go:build unix

package shim

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureDiscoverLog routes slog to a DEBUG-level buffer for the test. Not
// parallel: slog.SetDefault is process-global.
func captureDiscoverLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// liveShimLines counts the "discovered live shim" lines for key at level.
func liveShimLines(buf *bytes.Buffer, level, key string) int {
	n := 0
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "level="+level+" ") && strings.Contains(line, `msg="discovered live shim"`) &&
			strings.Contains(line, "key="+key+" ") {
			n++
		}
	}
	return n
}

// TestDiscover_LogsLiveShimAtInfoOnlyWhenTheSetChanges: the reconcile loop
// calls Discover every tick, so a shim already reported logs at DEBUG and
// changed stays false; a shim joining or leaving the set flips changed and
// only the newcomer logs at INFO.
func TestDiscover_LogsLiveShimAtInfoOnlyWhenTheSetChanges(t *testing.T) {
	buf := captureDiscoverLog(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "live.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := mustNewManager(t, ManagerConfig{StateDir: dir})

	discover := func(step string, wantFound int, wantChanged bool) {
		t.Helper()
		buf.Reset()
		states, changed, err := m.Discover()
		if err != nil {
			t.Fatalf("%s: Discover: %v", step, err)
		}
		if len(states) != wantFound || changed != wantChanged {
			t.Fatalf("%s: found %d, changed %v; want %d, %v", step, len(states), changed, wantFound, wantChanged)
		}
	}

	discover("first, empty dir", 0, true)
	discover("second, empty dir", 0, false)

	writeTestState(t, dir, State{ShimPID: os.Getpid(), Socket: sock, Key: "t:a"})
	if es, err := m.Inspect(); err == nil && len(es) == 1 && es[0].IdentityErr != nil {
		t.Skipf("binary identity unavailable here: %v", es[0].IdentityErr)
	}
	discover("a appears", 1, true)
	if n := liveShimLines(buf, "INFO", "t:a"); n != 1 {
		t.Errorf("a appears: %d INFO lines for t:a, want 1", n)
	}

	discover("steady", 1, false)
	if i, d := liveShimLines(buf, "INFO", "t:a"), liveShimLines(buf, "DEBUG", "t:a"); i != 0 || d != 1 {
		t.Errorf("steady tick: %d INFO and %d DEBUG lines for t:a, want 0 and 1", i, d)
	}

	pathB := writeTestState(t, dir, State{ShimPID: os.Getpid(), Socket: sock, Key: "t:b"})
	discover("b appears", 2, true)
	if n := liveShimLines(buf, "INFO", "t:b"); n != 1 {
		t.Errorf("b appears: %d INFO lines for t:b, want 1", n)
	}
	if n := liveShimLines(buf, "INFO", "t:a"); n != 0 {
		t.Errorf("b appears: %d INFO lines for the known t:a, want 0", n)
	}

	RemoveStateFile(pathB)
	discover("b leaves", 1, true)
	discover("steady after b", 1, false)
}

// TestNoteLive_PIDChangeIsAChange: a key back under another PID is a new shim
// (the old one exited and a respawn took its place), so it reports at INFO.
func TestNoteLive_PIDChangeIsAChange(t *testing.T) {
	buf := captureDiscoverLog(t)
	m := mustNewManager(t, ManagerConfig{StateDir: t.TempDir()})
	if !m.noteLive([]State{{Key: "t:a", ShimPID: 100}}) {
		t.Fatal("first set: changed = false")
	}
	if m.noteLive([]State{{Key: "t:a", ShimPID: 100}}) {
		t.Fatal("same key and PID: changed = true")
	}
	buf.Reset()
	if !m.noteLive([]State{{Key: "t:a", ShimPID: 101}}) {
		t.Fatal("same key, new PID: changed = false")
	}
	if n := liveShimLines(buf, "INFO", "t:a"); n != 1 {
		t.Errorf("new PID: %d INFO lines, want 1", n)
	}
}
