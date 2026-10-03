package project

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScan_LogsAtInfoOnlyWhenTheCountChanges: the server rescans every
// minute, so "scanned projects" is INFO on the first scan and whenever the
// count moves, and DEBUG on a rescan that finds the same number. Not
// parallel: slog.SetDefault is process-global.
func TestScan_LogsAtInfoOnlyWhenTheCountChanges(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	root := t.TempDir()
	m, err := NewManager(root, PlannerDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	scan := func(step, want string) {
		t.Helper()
		buf.Reset()
		if err := m.Scan(); err != nil {
			t.Fatalf("%s: Scan: %v", step, err)
		}
		n := 0
		for line := range strings.SplitSeq(buf.String(), "\n") {
			if strings.Contains(line, `msg="scanned projects"`) && strings.Contains(line, root) {
				n++
				if !strings.Contains(line, "level="+want+" ") {
					t.Errorf("%s: %q, want level %s", step, line, want)
				}
			}
		}
		if n != 1 {
			t.Errorf("%s: %d scanned-projects lines, want 1:\n%s", step, n, buf.String())
		}
	}
	mkdir := func(name string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	scan("first scan, no projects", "INFO")
	scan("rescan, same count", "DEBUG")
	mkdir("a")
	scan("a added", "INFO")
	scan("rescan after a", "DEBUG")
	if err := os.Remove(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	mkdir("b")
	scan("a swapped for b, same count", "DEBUG")
}
