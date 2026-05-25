package session

import (
	"os"
	"strings"
	"testing"
)

// TestTestUtilHasReleaseBuildTag pins the `//go:build !release` constraint
// on testutil.go so a future refactor cannot accidentally drop it and ship
// TestProcess + Router.InjectSession into a release binary.
//
// R246-ARCH-8 / R234-ARCH-18 / R239-ARCH-O resolution: the constraint is
// what gates testutil.go out of `go build -tags release` — without it,
// production binaries link the test stub and any plugin-loaded code
// reaching `cli.SubagentLinker == nil` paths could cast through it. The
// constraint is invisible to grep (no string occurrence in production
// code) so a contract test is the natural place to lock it.
//
// Approach: read testutil.go's first 32 bytes verbatim and assert the
// exact build-tag prefix. Reading raw avoids Go's tooling skipping the
// file under the test build (the `!release` tag is satisfied here, so
// the file *is* compiled into this test, but we still want to verify
// the source-level directive is present).
func TestTestUtilHasReleaseBuildTag(t *testing.T) {
	const path = "testutil.go"
	const want = "//go:build !release"

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// First non-empty line must be the build tag — Go requires build
	// constraints precede the package clause and any blank lines.
	lines := strings.SplitN(string(data), "\n", 4)
	if len(lines) == 0 {
		t.Fatalf("%s is empty", path)
	}
	if got := strings.TrimSpace(lines[0]); got != want {
		t.Errorf("%s first line = %q; want %q (R246-ARCH-8 contract: "+
			"testutil.go must be excluded from release builds via "+
			"//go:build !release; see godoc in testutil.go for rationale)",
			path, got, want)
	}
}
