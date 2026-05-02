package claudejsonl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/history"
)

// projDirName mirrors discovery.projDirName for test setup. Kept as a
// local test helper so this package doesn't import test-only code from
// another package.
func projDirName(cwd string) string {
	// The real projDirName replaces os.PathSeparator with '-'. We only
	// feed it absolute paths under /tmp/... which contain no slashes
	// that need escaping beyond the leading one, so a simple replace
	// is equivalent for test inputs.
	out := make([]byte, 0, len(cwd))
	for i := 0; i < len(cwd); i++ {
		c := cwd[i]
		if c == '/' || c == os.PathSeparator {
			out = append(out, '-')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

func makeClaudeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeSessionJSONL(t *testing.T, claudeDir, dirName, sessionID string, lines []string) {
	t.Helper()
	projDir := filepath.Join(claudeDir, "projects", dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projDir, sessionID+".jsonl")
	buf := ""
	for _, l := range lines {
		buf += l + "\n"
	}
	if err := os.WriteFile(path, []byte(buf), 0o644); err != nil {
		t.Fatal(err)
	}
}

func userLineAt(content string, unixSec int64) string {
	ts := time.Unix(unixSec, 0).UTC().Format(time.RFC3339)
	msg, _ := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: "user", Content: content})
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":%s}`, ts, string(msg))
}

func TestSource_ImplementsInterface(t *testing.T) {
	t.Parallel()
	// Compile-time check.
	var _ history.Source = (*Source)(nil)
}

func TestSource_LoadBefore_WalksChain(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/cjsonl-walk"
	dirName := projDirName(cwd)

	oldID := "11111111-1111-1111-1111-111111111bb1"
	newID := "22222222-2222-2222-2222-222222222bb2"

	oldLines := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		oldLines = append(oldLines, userLineAt(fmt.Sprintf("old-%d", i), int64(1000+i)))
	}
	writeSessionJSONL(t, claudeDir, dirName, oldID, oldLines)

	newLines := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		newLines = append(newLines, userLineAt(fmt.Sprintf("new-%d", i), int64(2000+i)))
	}
	writeSessionJSONL(t, claudeDir, dirName, newID, newLines)

	chainCalls := 0
	src := New(claudeDir, cwd, func() []string {
		chainCalls++
		return []string{oldID, newID} // oldest → newest
	})

	// beforeMS inside the newer session; expect 2 from newer + 3 from older.
	beforeMS := int64(2002) * 1000
	entries, err := src.LoadBefore(context.Background(), beforeMS, 5)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("entries=%d, want 5", len(entries))
	}
	for i, e := range entries {
		if e.Time >= beforeMS {
			t.Errorf("entries[%d].Time=%d not strictly < %d", i, e.Time, beforeMS)
		}
	}
	if chainCalls != 1 {
		t.Errorf("chain callback invoked %d times, want exactly 1 per LoadBefore", chainCalls)
	}
}

func TestSource_LoadBefore_ChainCallbackReevaluated(t *testing.T) {
	// Confirm the callback is re-invoked on each LoadBefore so chain updates
	// (e.g. a /new spawns a fresh session ID) are visible to the next page.
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/cjsonl-refresh"
	dirName := projDirName(cwd)

	id := "33333333-3333-3333-3333-333333333bb3"
	lines := []string{userLineAt("only", 1500)}
	writeSessionJSONL(t, claudeDir, dirName, id, lines)

	// First call returns empty chain — nothing to read. Second call supplies
	// the real chain — entries should appear.
	call := 0
	src := New(claudeDir, cwd, func() []string {
		call++
		if call == 1 {
			return nil
		}
		return []string{id}
	})

	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil || got != nil {
		t.Fatalf("first call want (nil, nil), got %v, %v", got, err)
	}
	got, err = src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if len(got) != 1 || got[0].Summary != "only" {
		t.Errorf("second call = %v, want 1 entry 'only'", got)
	}
}

func TestSource_LoadBefore_DegradesOnMisconfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		src       *Source
		limit     int
		wantError bool
	}{
		{"nil receiver", (*Source)(nil), 10, false},
		{"empty claudeDir", New("", "/tmp/x", func() []string { return []string{"x"} }), 10, false},
		{"nil chainIDs", New("/tmp/claude", "/tmp/x", nil), 10, false},
		{"limit zero", New("/tmp/claude", "/tmp/x", func() []string { return []string{"x"} }), 0, false},
		{"limit negative", New("/tmp/claude", "/tmp/x", func() []string { return []string{"x"} }), -5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.src.LoadBefore(context.Background(), 1000, tc.limit)
			if tc.wantError != (err != nil) {
				t.Errorf("err=%v, wantError=%v", err, tc.wantError)
			}
			if got != nil {
				t.Errorf("misconfig must yield nil entries, got %d", len(got))
			}
		})
	}
}

func TestSource_LoadBefore_EmptyChain(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	src := New(claudeDir, "/tmp/x", func() []string { return nil })
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("empty chain must yield nil, got %d entries", len(got))
	}
}
