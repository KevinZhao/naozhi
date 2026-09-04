// Package tuningspec holds the model / thinking-effort value validators shared
// by the config layer (startup validation) and the session layer (runtime
// per-session tuning overrides). It is a LEAF package because config already
// imports session, so session importing config back would cycle
// (docs/rfc/dashboard-model-effort-control.md §4.3 / §4.6).
package tuningspec

import (
	"fmt"
	"regexp"
)

// modelNameRe pins the first char to [A-Za-z0-9] so a leading `-` cannot be
// re-parsed by the CLI flag parser when naozhi assembles `--model <value>`
// (flag-injection guard). `[` `]` are allowed in the tail because the CLI's
// context-window suffix ("us.anthropic.claude-fable-5-1[1m]") is a real model
// id. Not shell-expanded — naozhi execs directly.
var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\[\]-]*$`)

// EffortTiers is the closed set of thinking-effort tiers naozhi will forward,
// matching `kiro-cli acp --effort`. An allowlist (unlike the display read
// path) because naozhi is constructing argv, so the same flag-injection
// argument applies; a typo'd tier must fail here rather than in kiro.
var EffortTiers = map[string]struct{}{
	"low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
}

// ValidateEffort gates a thinking-effort tier. Empty means "pass no flag";
// field names the source in the error (e.g. `cli.effort`).
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

// ValidateModel gates a model identifier. Empty is allowed; non-empty values
// must match modelNameRe and stay under 128 chars.
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
