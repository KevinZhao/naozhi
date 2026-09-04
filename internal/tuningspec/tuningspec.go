// Package tuningspec holds the model / thinking-effort value validators
// shared by the config layer (startup validation of cli.model /
// cli.backends[].effort / agents[].effort) and the session layer (runtime
// validation of dashboard per-session tuning overrides + sessions.json
// load-time re-validation).
//
// It is a LEAF package (no naozhi imports) because the two consumers sit on
// opposite sides of the config→session dependency edge: internal/config
// already reads internal/session exports, so session importing config back
// would cycle. Extracting the validators here follows the textutil
// precedent. docs/rfc/dashboard-model-effort-control.md §4.3 / §4.6;
// original validators: R215-SEC-P2-1 (model), kiro-effort-control §4.3
// (effort).
package tuningspec

import (
	"fmt"
	"regexp"
)

// modelNameRe pins the first char to [A-Za-z0-9] so a leading `-` (or `.`,
// ambiguous on some shells) is rejected outright — a value like "-foo"
// could otherwise be re-parsed by the CLI's flag parser when naozhi
// assembles argv (`--model <value>`). Flag-injection guard, R215-SEC-P2-1.
//
// `[` `]` are allowed in the tail because the claude CLI's context-window
// suffix syntax ("us.anthropic.claude-fable-5-1[1m]") is a real model id:
// the CLI echoes it in its init frame, the observed-model manifest serves
// it back to the popover, and the operator picks it. Rejecting it turned
// every popover pick into a 400. Not shell-expanded — naozhi execs directly.
var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\[\]-]*$`)

// EffortTiers is the closed set of thinking-effort tiers naozhi will
// forward, matching `kiro-cli acp --effort` (verified 2.16.0–2.20.2).
//
// Why an allowlist here when the dashboard read path deliberately avoids
// one: the read path displays a tier kiro has ALREADY chosen, so refusing
// to render an unfamiliar one would lose real information
// (kiro-effort-visibility.md §5 R4). Here naozhi is constructing argv, so
// the same flag-injection argument that pins modelNameRe applies — and a
// typo'd tier should fail at validation time rather than being silently
// misapplied by kiro. A future kiro tier requires extending this set; the
// error message says so.
var EffortTiers = map[string]struct{}{
	"low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
}

// ValidateEffort gates a thinking-effort tier. Empty is allowed and means
// "pass no flag" (backend keeps its own default). field names the source
// in the error (e.g. `cli.effort`, `session tuning effort`).
func ValidateEffort(field, value string) error {
	if value == "" {
		return nil
	}
	if _, ok := EffortTiers[value]; !ok {
		return fmt.Errorf("%s %q is not a known thinking-effort tier "+
			"(want one of: low, medium, high, xhigh, max) — if a newer kiro "+
			"added a tier, extend tuningspec.EffortTiers", field, value)
	}
	return nil
}

// ValidateModel gates a model identifier. Empty is allowed (caller-defined
// fallback semantics). Non-empty values must match modelNameRe and stay
// under 128 chars.
func ValidateModel(field, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 128 {
		return fmt.Errorf("%s %q is too long (max 128 chars)", field, value)
	}
	if !modelNameRe.MatchString(value) {
		return fmt.Errorf("%s %q must match [A-Za-z0-9._[]-]+ — refusing (flag-injection guard)", field, value)
	}
	return nil
}
