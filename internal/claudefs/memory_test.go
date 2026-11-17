package claudefs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEditMemoryIndex: only the lines linking exactly "(<name>.md)" go, the
// rest stay as they were (a leading heading and blank line included); a line
// is appended when given; a missing index is created only to add, and an
// index with nothing to drop is left untouched.
func TestEditMemoryIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MemoryIndexFile)
	if n, err := EditMemoryIndex(path, "a", ""); n != 0 || err != nil {
		t.Fatalf("drop from a missing index = %d, %v", n, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a drop created the index: %v", err)
	}
	if _, err := EditMemoryIndex(path, "a", "- [a](a.md)"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("\n# 索引\n\n- [a](a.md)\n- [a2](a.md.bak)\n- [b](not-a.md)\n- 又是 (a.md)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := EditMemoryIndex(path, "a", "- [c](c.md)")
	if err != nil || n != 2 {
		t.Fatalf("EditMemoryIndex = %d, %v", n, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "\n# 索引\n\n- [a2](a.md.bak)\n- [b](not-a.md)\n- [c](c.md)\n" {
		t.Fatalf("index = %q", data)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want the index's own 0600", st.Mode().Perm())
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if n, err := EditMemoryIndex(path, "zzz", ""); n != 0 || err != nil {
		t.Fatalf("drop of an unlisted name = %d, %v", n, err)
	}
	if st, _ := os.Stat(path); !st.ModTime().Equal(old) {
		t.Error("an index with nothing to drop was rewritten")
	}
}

func TestPlannerWrittenMemory(t *testing.T) {
	for data, want := range map[string]bool{
		"---\nname: a\nmetadata:\n  source: naozhi-planner\n---\nx\n": true,
		"---\nname: a\n---\nsource: naozhi-planner\n":                 false,
		"no frontmatter source: naozhi-planner":                       false,
	} {
		if got := PlannerWrittenMemory([]byte(data)); got != want {
			t.Errorf("PlannerWrittenMemory(%q) = %v", data, got)
		}
	}
}
