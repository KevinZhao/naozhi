// workdir_resolve_shared_doc_test.go pins the SHARED-ALGORITHM-WITH-SERVER
// cross-reference comment in scheduler.go's workDirResolveUnderRoot godoc.
// R20260527122801-ARCH-4 / #1316: cron and server have twin EvalSymlinks
// → prefix-check implementations that must evolve together. Without a
// machine-checked anchor a future refactor can silently delete the
// cross-reference and re-open the drift window the comment was added to
// close. The test reads scheduler.go's source and asserts the SHARED-
// ALGORITHM marker is present in the workDirResolveUnderRoot godoc block.
//
// Lives under internal/cron/ so the test only fires when something
// inside the cron package's view of the contract changes; the matching
// counter-test on server's side is tracked by the same issue.
package cron

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestWorkDirResolveUnderRoot_SharedAlgorithmCrossRef(t *testing.T) {
	src, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatalf("read scheduler.go: %v", err)
	}
	// Locate the workDirResolveUnderRoot godoc block: everything from the
	// last `^// workDirResolveUnderRoot ` line up to the matching
	// `^func workDirResolveUnderRoot` line.
	doc := regexp.MustCompile(`(?ms)^// workDirResolveUnderRoot is the variant.*?\nfunc workDirResolveUnderRoot\(`)
	m := doc.Find(src)
	if m == nil {
		t.Fatal("scheduler.go: workDirResolveUnderRoot godoc block not found — refactor renamed the function?")
	}
	preamble := string(m)
	must := []string{
		"SHARED-ALGORITHM-WITH-SERVER",
		"#1316",
		"validateWorkspace",
	}
	for _, needle := range must {
		if !strings.Contains(preamble, needle) {
			t.Errorf("workDirResolveUnderRoot godoc missing %q — the cron↔server "+
				"twin-algorithm cross-reference must stay so future dedup passes "+
				"see both sites at once (R20260527122801-ARCH-4 / #1316)", needle)
		}
	}
}
