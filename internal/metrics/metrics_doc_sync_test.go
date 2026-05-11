package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestCountersDocSyncedWithPprofMd pins that the counter name set documented
// in docs/ops/pprof.md matches the set declared in metrics.go one-to-one.
// RNEW-OPS-416: we added 8 new counters (WSAuthFailRate/InvalidToken,
// SpawnPanicRecovered, ShimReconnectGraceBackfill, Interrupt×4) over time but
// the runbook table froze at the original 5 names, so operators reading the
// doc cannot tell why their scrape output has extra fields. Keep the two
// sources in lock-step: a new counter must ship with a doc row; a rename
// must update the doc; both sides fail loud on drift.
func TestCountersDocSyncedWithPprofMd(t *testing.T) {
	t.Parallel()

	// Locate the repo root by walking up from this test file. The test
	// binary runs with a working directory of internal/metrics; pprof.md
	// lives at docs/ops/pprof.md off the repo root.
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(self), "..", ".."))
	pprofMd := filepath.Join(repoRoot, "docs", "ops", "pprof.md")

	body, err := os.ReadFile(pprofMd)
	if err != nil {
		t.Fatalf("read %s: %v", pprofMd, err)
	}

	// Pull every backtick-quoted `naozhi_*_total` identifier in the file.
	// The regex intentionally matches both the name column and any in-text
	// references (e.g. the "pair naozhi_shim_restart_total" suggestion in the
	// alert-cue column); docSet is a map so the duplicate mentions collapse
	// into a single set entry and the equality check stays clean.
	tableRow := regexp.MustCompile("`(naozhi_[a-z0-9_]+_total)`")
	matches := tableRow.FindAllSubmatch(body, -1)
	docSet := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		docSet[string(m[1])] = struct{}{}
	}

	// All counter names declared in metrics.go live in expvar.NewInt("...").
	metricsGo := filepath.Join(repoRoot, "internal", "metrics", "metrics.go")
	src, err := os.ReadFile(metricsGo)
	if err != nil {
		t.Fatalf("read %s: %v", metricsGo, err)
	}
	srcDecl := regexp.MustCompile(`expvar\.NewInt\("(naozhi_[a-z0-9_]+_total)"\)`)
	decls := srcDecl.FindAllSubmatch(src, -1)
	codeSet := make(map[string]struct{}, len(decls))
	for _, m := range decls {
		codeSet[string(m[1])] = struct{}{}
	}

	if len(codeSet) == 0 {
		t.Fatalf("metrics.go: no expvar.NewInt declarations matched — regex out of sync with source?")
	}

	var missingInDoc, extraInDoc []string
	for name := range codeSet {
		if _, ok := docSet[name]; !ok {
			missingInDoc = append(missingInDoc, name)
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
		t.Errorf("counters in metrics.go but missing from docs/ops/pprof.md table:\n  %s\nadd a row to the expvar 计数器 table documenting semantics + alert threshold.", strings.Join(missingInDoc, "\n  "))
	}
	if len(extraInDoc) > 0 {
		t.Errorf("counters in docs/ops/pprof.md but not declared in metrics.go:\n  %s\ndoc rows for renamed/removed counters must be deleted or the code must be restored.", strings.Join(extraInDoc, "\n  "))
	}
}
