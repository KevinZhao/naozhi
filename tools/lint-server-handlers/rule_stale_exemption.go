// rule 5 (stale_exemption): a file_size exemption must name a file that exists
// AND carry an expiry date that has not passed.
//
// It used to check existence only, and the validity window was `until_phase`.
// Every phase those entries pointed at had been shelved by ADR-001, so
// "until Phase 5" meant "forever" and nothing ever forced a re-decision — the
// exemption list only grew. #2561 replaced the key with an absolute date: a date
// cannot be shelved, and this rule fails the build once it passes.
//
// A missing or unparseable date is also a violation, so a new entry cannot dodge
// the ratchet by leaving the field out.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// exemptionDateLayout is the `until` format: a plain date, no clock, no zone.
// Exemption horizons are decided in weeks, and a timestamp would invite
// arguments about which timezone the build ran in.
const exemptionDateLayout = "2006-01-02"

// scanStaleExemption implements rule 5. now is injectable so the negative tests
// can drive an expiry without waiting for the calendar.
func scanStaleExemption(exempts *exemptions, now time.Time) []Violation {
	var out []Violation
	for _, e := range exempts.FileSize {
		if _, err := os.Stat(e.Path); err != nil && os.IsNotExist(err) {
			out = append(out, Violation{
				Rule: "stale_exemption",
				File: filepath.ToSlash(e.Path),
				Message: fmt.Sprintf("exemption entry references non-existent file %q (until: %s) — entry should be removed; the PR that removed the file should carry 'Closes-exemption: %s'",
					e.Path, e.Until, e.Path),
			})
			continue
		}
		if e.Until == "" {
			out = append(out, Violation{
				Rule:    "stale_exemption",
				File:    filepath.ToSlash(e.Path),
				Message: "exemption entry has no `until:` date — an exemption without an expiry is permanent, which is the failure mode #2561 removed. Set until: YYYY-MM-DD and name the issue that will retire it",
			})
			continue
		}
		until, err := time.Parse(exemptionDateLayout, e.Until)
		if err != nil {
			out = append(out, Violation{
				Rule:    "stale_exemption",
				File:    filepath.ToSlash(e.Path),
				Message: fmt.Sprintf("exemption `until: %s` is not a YYYY-MM-DD date (%v)", e.Until, err),
			})
			continue
		}
		if now.After(until) {
			out = append(out, Violation{
				Rule: "stale_exemption",
				File: filepath.ToSlash(e.Path),
				Message: fmt.Sprintf("exemption for %q expired on %s — either shrink the file below its limit and delete the entry, or make a fresh decision and extend the date with a reason. Do not extend it silently",
					e.Path, e.Until),
			})
		}
	}
	return out
}
