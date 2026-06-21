package cron_test

import (
	"os/exec"
	"strings"
	"testing"
)

// no_agentcore_import_test.go pins the [R202606-ARCH-1] invariant: internal/cron
// must NOT depend on internal/agentcore (nor, transitively, the AWS SDK) in
// production code.
//
// Background: sandbox.go declares an isolation contract (godoc on
// SandboxStateSuccess / SandboxRunMeta) that "the scheduler stays compile-time
// independent of the AWS SDK — the wireup layer owns that edge". A single
// `const sandboxEventsMaxLineSize = agentcore.MaxEnvelopeLineBytes` had quietly
// pinned the cron→agentcore import edge, dragging the whole aws-sdk-go-v2 graph
// into every cron production build. The fix derives the same ceiling from the
// shared leaf package (limits.MaxStreamJSONLine + 64KiB) instead.
//
// This test mirrors no_platform_import_test.go's transitive-closure form (not a
// source grep): `go list -deps` walks the actual build graph so build-tag tricks
// cannot bypass it, and _test.go imports are excluded.
const (
	cronPkgForAgentcore = "github.com/naozhi/naozhi/internal/cron"
	agentcorePkg        = "github.com/naozhi/naozhi/internal/agentcore"
	awsSDKPkgPrefix     = "github.com/aws/aws-sdk-go-v2"
)

// TestCron_NoImport_Agentcore asserts internal/cron's transitive dependency
// closure does NOT include internal/agentcore or the AWS SDK.
func TestCron_NoImport_Agentcore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping `go list -deps` walk in -short mode")
	}
	out, err := exec.Command("go", "list", "-deps", cronPkgForAgentcore).Output()
	if err != nil {
		// `go list` failure usually means no go in PATH (sandboxed CI) or a
		// broken module graph. Skip rather than fail so a transient tooling
		// issue doesn't mask real regressions elsewhere.
		t.Skipf("go list failed (skipping import-graph check): %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep := strings.TrimSpace(line)
		if dep == agentcorePkg {
			t.Fatalf("internal/cron transitively imports %s — [R202606-ARCH-1] requires cron to "+
				"stay compile-time independent of the AWS SDK. Derive shared constants from leaf "+
				"packages (e.g. limits.MaxStreamJSONLine) instead of importing agentcore directly.", agentcorePkg)
		}
		if strings.HasPrefix(dep, awsSDKPkgPrefix) {
			t.Fatalf("internal/cron transitively imports %s — [R202606-ARCH-1] requires the AWS SDK "+
				"stay out of the cron build graph. The wireup layer owns that edge.", dep)
		}
	}
}
