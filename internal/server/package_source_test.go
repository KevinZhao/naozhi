package server

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
// in filename order (#2560).
//
// A dozen contract tests here read source by HARD-CODED FILENAME —
// os.ReadFile("send.go") and friends — to assert things like "the workspace
// value passes through SanitizeForLog before it is logged". The assertion is
// about the package's behaviour, but the lookup is about a filename, so moving a
// function between files in the same package breaks the gate for no reason. That
// cost is not theoretical: it fired three times in one day of refactoring
// (#2551 moved sessionSend's receiver and hit log_level_test.go plus
// send_sanitize_contract_test.go; #2554 moved the cross-origin warn out of
// handlers.go and hit auth_log_sanitize_contract_test.go). Each time the "fix"
// was to point the test at the new filename — which teaches nothing and leaves
// the next move to pay again.
//
// Reading the whole package removes the failure mode. The gates still fail when
// the SANITISER disappears, which is what they are for, and stop failing when
// code merely relocates.
//
// Where a test genuinely asserts file ownership ("this must be declared in
// wshub.go so the field-block contract holds"), the filename IS the assertion
// and those tests keep reading one file on purpose.
func packageGoSource(t *testing.T) string {
	t.Helper()
	return cachedPackageSource(t, packageDir(t))
}

// packageDir is the directory of the calling test file, i.e. the package dir
// under `go test`.
func packageDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(self)
}

var (
	pkgSrcOnce sync.Once
	pkgSrc     string
	pkgSrcErr  error
	pkgSrcN    int
)

// cachedPackageSource reads the package once; ~40 files × ~12k lines is cheap
// but a dozen tests each re-reading it in parallel is pointless I/O.
func cachedPackageSource(t *testing.T, dir string) string {
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
			// A marker per file so a failure message can say where it looked,
			// and so a pattern cannot accidentally match across a file boundary.
			b.WriteString("\n// ===== " + n + " =====\n")
			b.Write(data)
		}
		pkgSrc = b.String()
		pkgSrcN = len(names)
	})
	if pkgSrcErr != nil {
		t.Fatalf("read package source: %v", pkgSrcErr)
	}
	// Guard against a vacuous pass: an empty or near-empty read would make every
	// "source must contain X" assertion fail loudly, but a "source must NOT
	// contain Y" assertion would pass for the wrong reason.
	if pkgSrcN < 10 || len(pkgSrc) < 10_000 {
		t.Fatalf("package source scan looks wrong: %d files / %d bytes — negative assertions would pass vacuously",
			pkgSrcN, len(pkgSrc))
	}
	return pkgSrc
}
