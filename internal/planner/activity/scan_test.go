package activity

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestScan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start := time.Now().Add(-time.Minute)
	old := start.Add(-time.Hour)
	files := []struct {
		rel string
		old bool
	}{
		{"report.html", false},
		{"chart.PNG", false},
		{"a/b/c/deep.md", false},       // depth 3: read
		{"a/b/c/d/too-deep.md", false}, // depth 4: past the bound
		{"stale.md", true},
		{"main.go", false},
		{".hidden.md", false},
		{".git/x.md", false},
		{"node_modules/p/readme.md", false},
		{"vendor/v/readme.md", false},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if f.old {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, p := range Scan(context.Background(), dir, start) {
		rel, _ := filepath.Rel(dir, p)
		got = append(got, filepath.ToSlash(rel))
	}
	want := []string{"chart.PNG", "report.html", "a/b/c/deep.md"}
	if !slices.Equal(got, want) {
		t.Errorf("Scan = %v, want %v", got, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := Scan(ctx, dir, start); len(got) != 0 {
		t.Errorf("canceled Scan = %v, want none", got)
	}
}
