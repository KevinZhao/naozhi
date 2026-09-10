package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// packageGoSource returns every non-test .go file in this package concatenated,
// in filename order.
//
// dashboard_cron_missed_cache_test.go read source by hard-coded filename —
// os.ReadFile("handlers.go") — to assert that HandleList routes through
// missedScheduleVerdict. #2561 split handlers.go from 1,296 lines into six files,
// HandleList moved to list_preview.go, and the gate failed with "not found in
// handlers.go". The assertion is about the package; the lookup was about a
// filename.
//
// Third package to need this helper: #2560 added it to internal/server after the
// identical failure fired three times in one day, and #2561's own file splits
// then hit it in dashsession and here. Repointing the filename each time would
// have worked and taught nothing.
func packageGoSource(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return cachedPkgSource(t, filepath.Dir(self))
}

var (
	pkgSrcOnce sync.Once
	pkgSrc     string
	pkgSrcErr  error
	pkgSrcN    int
)

func cachedPkgSource(t *testing.T, dir string) string {
	t.Helper()
	pkgSrcOnce.Do(func() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			pkgSrcErr = err
			return
		}
		var names []string
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			names = append(names, n)
		}
		sort.Strings(names)
		var b strings.Builder
		for _, n := range names {
			data, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				pkgSrcErr = err
				return
			}
			b.WriteString("\n// ===== " + n + " =====\n")
			b.Write(data)
		}
		pkgSrc = b.String()
		pkgSrcN = len(names)
	})
	if pkgSrcErr != nil {
		t.Fatalf("read package source: %v", pkgSrcErr)
	}
	// Fail loudly rather than let a broken scan satisfy every "must NOT contain"
	// assertion vacuously.
	if pkgSrcN < 4 || len(pkgSrc) < 5_000 {
		t.Fatalf("package source scan looks wrong: %d files / %d bytes", pkgSrcN, len(pkgSrc))
	}
	return pkgSrc
}
