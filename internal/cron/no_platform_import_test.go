package cron_test

import (
	"os/exec"
	"strings"
	"testing"
)

// no_platform_import_test.go pins the #725 invariant: internal/cron must NOT
// depend on internal/platform in production code.
//
// Background: notifyTarget historically indexed a map[string]platform.Platform
// and called platform.SplitText / platform.ReplyWithRetry directly, which
// pinned the cron→platform import edge. #725 introduced the cron-local
// NotifySender / PlatformReplier interfaces (notify_sender.go) and moved the
// concrete platform translation into internal/wireup/cron_notify_sender.go,
// mirroring the cron→session inversion that cron_no_session_import_test.go
// guards (RFC cron-sysession-merge Phase B / #1318).
//
// Without this contract test a future cron-side change that reaches for any
// platform.* symbol would silently re-introduce the reverse import (the build
// still succeeds — Go does not flag edges that pass through the wireup
// boundary). This test runs `go list -deps` on internal/cron's NON-test
// closure and fails if internal/platform reappears. It deliberately mirrors
// cron_no_session_import_test.go's transitive-closure form (not a source
// grep): `go list -deps` walks the actual build graph so build-tag tricks
// cannot bypass it, and _test.go imports are excluded (the test fakes may
// still import platform for SplitText/ReplyWithRetry without re-pinning the
// production edge).
const (
	cronPkg     = "github.com/naozhi/naozhi/internal/cron"
	platformPkg = "github.com/naozhi/naozhi/internal/platform"
)

// TestCron_NoReverseImport_Platform asserts internal/cron's transitive
// dependency closure does NOT include internal/platform.
func TestCron_NoReverseImport_Platform(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping `go list -deps` walk in -short mode")
	}
	out, err := exec.Command("go", "list", "-deps", cronPkg).Output()
	if err != nil {
		// `go list` failure usually means no go in PATH (sandboxed CI) or a
		// broken module graph. Skip rather than fail so a transient tooling
		// issue doesn't mask real regressions elsewhere.
		t.Skipf("go list failed (skipping import-graph check): %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == platformPkg {
			t.Fatalf("internal/cron transitively imports %s — #725 requires cron→platform "+
				"inversion. Route platform calls through the cron.NotifySender / "+
				"PlatformReplier interfaces and build the adapter in "+
				"internal/wireup/cron_notify_sender.go instead of importing platform directly.", platformPkg)
		}
	}
}
