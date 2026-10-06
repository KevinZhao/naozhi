package metrics

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestCountersDocSyncedWithPprofMd pins docs/ops/pprof.md to the metrics
// registered by non-test code in any package (counters and gauges alike).
// Every registered metric needs a table row of its own, keyed by its first
// column; every backticked naozhi_* name anywhere in the doc must still be
// registered, so a rename cannot leave a stale mention behind.
func TestCountersDocSyncedWithPprofMd(t *testing.T) {
	t.Parallel()

	pprofMd := filepath.Join(repoRoot(t), "docs", "ops", "pprof.md")
	body, err := os.ReadFile(pprofMd)
	if err != nil {
		t.Fatalf("read %s: %v", pprofMd, err)
	}

	// rowSet holds first-column names only: a mention in another metric's
	// alert cue is not documentation. docSet holds every mention.
	rowName := regexp.MustCompile("(?m)^\\|\\s*`(naozhi_[a-z0-9_]+)`\\s*\\|")
	rowSet := make(map[string]struct{})
	for _, m := range rowName.FindAllSubmatch(body, -1) {
		rowSet[string(m[1])] = struct{}{}
	}
	docName := regexp.MustCompile("`(naozhi_[a-z0-9_]+)`")
	docSet := make(map[string]struct{})
	for _, m := range docName.FindAllSubmatch(body, -1) {
		docSet[string(m[1])] = struct{}{}
	}

	codeSet := declaredMetricNames(t)
	// Canary: a scan narrowed back to internal/metrics would pass vacuously
	// for every metric registered elsewhere.
	if got := codeSet["naozhi_dispatch_message_total"]; got != "internal/dispatch/metrics.go" {
		t.Fatalf("naozhi_dispatch_message_total declared in %q, want internal/dispatch/metrics.go; is the repo scan still repo-wide?", got)
	}

	var missingInDoc, extraInDoc []string
	for name, file := range codeSet {
		if _, ok := rowSet[name]; !ok {
			missingInDoc = append(missingInDoc, name+" ("+file+")")
		}
	}
	for name := range docSet {
		if _, ok := codeSet[name]; !ok {
			extraInDoc = append(extraInDoc, name)
		}
	}
	sort.Strings(missingInDoc)
	sort.Strings(extraInDoc)

	if len(missingInDoc) > 0 {
		t.Errorf("metrics registered in code without a table row in docs/ops/pprof.md:\n  %s\nadd a row (semantics + alert cue) to the counter or gauge table.", strings.Join(missingInDoc, "\n  "))
	}
	if len(extraInDoc) > 0 {
		t.Errorf("metrics in docs/ops/pprof.md but registered nowhere in code:\n  %s\ndelete the rows of renamed/removed metrics or restore the code.", strings.Join(extraInDoc, "\n  "))
	}
}

// metricDecl matches a naozhi_* expvar registration in any package; the
// labeled forms (and promexport.NewMap) wrap expvar.NewMap.
var metricDecl = regexp.MustCompile(`(?:expvar\.NewInt|expvar\.NewMap|expvar\.NewFloat|promexport\.NewMap|NewLabeledCounter|NewLabeledGauge)\(\s*"(naozhi_[a-z0-9_]+)"`)

// repoRoot returns the checkout root this test file lives in.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(self), "..", ".."))
}

// declaredMetricNames maps every naozhi_* metric registered in a non-test .go
// file anywhere in the repo to the repo-relative file declaring it. Two
// declarations of one name are fatal: expvar would panic at init.
func declaredMetricNames(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	decls := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if testhelper.SkipRepoDir(root, path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(src, []byte(`"naozhi_`)) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, m := range metricDecl.FindAllSubmatch(src, -1) {
			metric := string(m[1])
			if prev, dup := decls[metric]; dup {
				t.Fatalf("%s is registered twice (%s and %s); expvar panics on a duplicate name", metric, prev, rel)
			}
			decls[metric] = rel
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return decls
}
