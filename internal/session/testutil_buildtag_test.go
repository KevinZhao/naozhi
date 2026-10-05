package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTestUtilHasReleaseBuildTag pins the `//go:build !release` constraint
// on every testutil*.go file so a future refactor cannot accidentally drop it
// and ship TestProcess + Router.InjectSession, or the fake-shim fixture, into
// a release binary.
//
// R246-ARCH-8 / R234-ARCH-18 / R239-ARCH-O resolution: the constraint is
// what gates testutil.go out of `go build -tags release` — without it,
// production binaries link the test stub and any plugin-loaded code
// reaching `subagent.Linker == nil` paths could cast through it. The
// constraint is invisible to grep (no string occurrence in production
// code) so a contract test is the natural place to lock it.
//
// Approach: read each file's first line verbatim and assert the
// exact build-tag prefix. Reading raw avoids Go's tooling skipping the
// file under the test build (the `!release` tag is satisfied here, so
// the file *is* compiled into this test, but we still want to verify
// the source-level directive is present).
func TestTestUtilHasReleaseBuildTag(t *testing.T) {
	const want = "//go:build !release"

	for _, path := range testutilSources(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// First non-empty line must be the build tag — Go requires build
		// constraints precede the package clause and any blank lines.
		lines := strings.SplitN(string(data), "\n", 4)
		if got := strings.TrimSpace(lines[0]); got != want {
			t.Errorf("%s first line = %q; want %q (R246-ARCH-8 contract: "+
				"testutil*.go must be excluded from release builds via "+
				"//go:build !release; see godoc in testutil.go for rationale)",
				path, got, want)
		}
	}
}

// testutilSources lists the non-test testutil*.go files: the cross-package
// test seams, each of which must carry the release exclusion.
func testutilSources(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("testutil*.go")
	if err != nil {
		t.Fatal(err)
	}
	var srcs []string
	for _, p := range all {
		if !strings.HasSuffix(p, "_test.go") {
			srcs = append(srcs, p)
		}
	}
	if !slices.Contains(srcs, "testutil.go") || !slices.Contains(srcs, "testutil_shim.go") {
		t.Fatalf("testutil sources = %v, want testutil.go and testutil_shim.go among them", srcs)
	}
	return srcs
}
