package session

import (
	"os"
	"strings"
	"testing"
)

// TestTestutil_BuildTagContract pins the production-binary protection on
// internal/session/testutil.go. The file deliberately uses the
// "testutil.go" name (not testutil_test.go) so cross-package consumers in
// internal/server/*_test.go and internal/upstream/*_test.go can reach
// TestProcess + Router.InjectSession — Go only compiles *_test.go files
// when the enclosing package is being tested, so a "_test.go" rename
// would hide those symbols from external test packages and break dozens
// of dashboard / wshub tests.
//
// The compromise R226-CR-14 / R227-CR-2 / R230-CQ-5 / R232-ARCH-4 /
// R234-ARCH-18 / R239-ARCH-O / R246-ARCH-8 settled on: keep the cross-
// package-friendly file name, but guard it with `//go:build !release`
// so a `go build -tags release` strips the test stub from the production
// binary. The TestProcess constructor + Router.InjectSession method are
// the link-time canaries; if anything reachable from cmd/naozhi/main.go
// ever references them, a release build fails fast at link time.
//
// This contract test asserts the build tag is present at the head of
// testutil.go so a future refactor that drops the tag (or migrates
// to a positive `//go:build testing` form without updating the file)
// fails the contract instead of silently shipping the test stub into
// every production binary by default. Migrating to a different tag
// shape is fine — update the regex below in the same patch.
func TestTestutil_BuildTagContract(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("testutil.go")
	if err != nil {
		t.Fatalf("read testutil.go: %v", err)
	}

	// Locate the first non-blank line: `//go:build` must appear at the very
	// top of the file (Go spec — build constraint must precede the package
	// clause and any blank line) before the package comment block. We
	// tolerate either `//go:build !release` (current chosen form) or
	// `//go:build release_excluded`-style alternatives so a future tag
	// rename only touches this regex.
	head := string(src)
	if !strings.HasPrefix(head, "//go:build ") {
		t.Fatalf("testutil.go does not start with `//go:build` constraint. " +
			"R234-ARCH-18 / R239-ARCH-O / R246-ARCH-8: this file ships " +
			"TestProcess + Router.InjectSession into the production binary " +
			"by default unless a build constraint excludes it. The chosen " +
			"approximation is `//go:build !release` so `-tags release` " +
			"strips the stub at link time. Either restore that line or " +
			"update this test to recognise the new exclusion shape.")
	}

	firstLineEnd := strings.IndexByte(head, '\n')
	if firstLineEnd < 0 {
		t.Fatal("testutil.go has no newline; build constraint cannot be parsed")
	}
	tag := strings.TrimSpace(head[len("//go:build "):firstLineEnd])

	// Reject a tag that does not exclude any builds at all (e.g. the empty
	// constraint or `any` — both compile in every config). The whole point
	// of the constraint is to give ops a release-only opt-out, so the tag
	// expression must exclude SOMETHING.
	if tag == "" || tag == "any" {
		t.Errorf("testutil.go //go:build %q does not exclude any build "+
			"configuration; the production-binary protection is moot. "+
			"R234-ARCH-18 / R239-ARCH-O: the chosen approximation is "+
			"`!release` (excluded under `-tags release`); pick another "+
			"shape only if it likewise excludes at least one realistic "+
			"build matrix entry.", tag)
	}

	// The package + the standard test-utility doc comment must still be
	// present — a refactor that drops the file's contents but leaves the
	// tag in place is also broken.
	if !strings.Contains(head, "package session") {
		t.Error("testutil.go no longer declares package session; the build " +
			"tag pin presumes the file still hosts the cross-package test " +
			"stub.")
	}
	for _, sym := range []string{"TestProcess", "InjectSession"} {
		if !strings.Contains(head, sym) {
			t.Errorf("testutil.go no longer references %s. The contract test "+
				"presumes the file still hosts the cross-package stub; if "+
				"the symbols moved to a subpackage, update this test or "+
				"remove it as part of the same migration.", sym)
		}
	}
}
