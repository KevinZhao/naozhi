package cli

import (
	"os"
	"regexp"
	"testing"
)

// TestACPProtocol_ProductionStatusDocumented pins R217-ARCH-9 (#629): the
// open question "is protocol_acp.go production-active or only a test/doc
// placeholder?" was answered "yes, kiro backend uses it via
// internal/cli/backend/profile_kiro.go". The answer lives at the top of
// protocol_acp.go so the next reviewer who walks the file does not have
// to re-derive it from grep + cross-package wiring traces.
//
// This contract test locks the doc so a future refactor that strips the
// header comment (or attempts to re-build-tag the file out) fails CI and
// forces a conscious re-review against the kiro wiring.
func TestACPProtocol_ProductionStatusDocumented(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("protocol_acp.go")
	if err != nil {
		t.Fatalf("read protocol_acp.go: %v", err)
	}
	src := string(data)

	// Anchor 1: the file must declare itself production-active (not a
	// placeholder). Tolerant to comment reflow via the multi-line dotall
	// flag.
	if !regexp.MustCompile(`(?i)production[ -]active`).MatchString(src) {
		t.Error("protocol_acp.go header must declare the file production-active to answer R217-ARCH-9")
	}

	// Anchor 2: the R217-ARCH-9 / #629 anchor must be present so the
	// triage trail back to the issue is preserved.
	if !regexp.MustCompile(`R217-ARCH-9`).MatchString(src) {
		t.Error("protocol_acp.go must reference R217-ARCH-9 anchor")
	}

	// Anchor 3: the doc must point at kiro as the live consumer so a
	// reader knows where to look (profile_kiro.go) without grep.
	if !regexp.MustCompile(`(?i)kiro`).MatchString(src) {
		t.Error("protocol_acp.go header must name the kiro backend as the live consumer")
	}

	// Anchor 4: the warning to NOT build-tag the file out must be
	// explicit. The proposal in the issue ("build-tag isolation") is the
	// wrong outcome given current production wiring; the doc must say so.
	if !regexp.MustCompile(`(?i)build-tag`).MatchString(src) {
		t.Error("protocol_acp.go header must warn against build-tag isolation given the kiro wiring")
	}
}
