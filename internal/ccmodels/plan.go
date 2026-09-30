package ccmodels

import "fmt"

// Status is a probe's conclusion about one alias.
type Status string

const (
	// StatusOK means the alias reached its profile and the profile answered.
	StatusOK Status = "ok"
	// StatusDenied means the profile cannot be invoked by this caller: no IAM
	// permission on it, or no such profile.
	StatusDenied Status = "denied"
	// StatusBadAlias means the alias never reached its profile: cc rewrote it
	// before consulting modelOverrides and silently used fallbackModel.
	StatusBadAlias Status = "bad-alias"
	// StatusUnknown means the probe could not decide — throttled, timed out, or
	// credentials unavailable. Treated as keepable.
	StatusUnknown Status = "unknown"
)

// Verdict is one probe result. Detail carries the operator-facing reason and is
// the only place a raw provider error string is surfaced.
type Verdict struct {
	Alias  string
	Status Status
	Detail string
	// Window is the context window the turn actually ran with, 0 when unmeasured.
	// A [1m] alias reporting less still works, so it becomes a Plan warning
	// rather than an exclusion.
	Window int
}

// keepable reports whether a verdict permits offering the alias. Absent and
// undecided both keep it: a probe outage must not strip a working model list.
func keepable(v Verdict, probed bool) bool {
	return !probed || v.Status == StatusOK || v.Status == StatusUnknown
}

// Repair records that a snapshot alias was replaced by the spelling derived
// from its own profile id.
type Repair struct {
	From, To string
}

// Plan is the reconciled model list: the availableModels / modelOverrides pair
// ccmodels owns in a settings document.
type Plan struct {
	Aliases  []Alias
	Excluded []Verdict
	Repaired []Repair
	// Warnings are kept aliases that behaved worse than advertised, reported so
	// an operator sees the degradation instead of wondering about it later.
	Warnings []string
	// Windows is the measured context window per offered alias, absent when the
	// probe did not measure one. Only picker row subtitles read it, so a missing
	// entry costs a detail rather than a model.
	Windows map[string]int
}

// oneMWindow is the context window a [1m] alias is supposed to deliver.
const oneMWindow = 1_000_000

// Candidates returns every alias a prober should test — each snapshot alias plus
// its profile-derived repair spelling — deduped, in snapshot order with each
// repair directly after its origin.
func Candidates(snap Snapshot) []Alias {
	seen := make(map[string]bool, len(snap.Aliases)*2)
	out := make([]Alias, 0, len(snap.Aliases)*2)
	add := func(a Alias) {
		if seen[a.Name] {
			return
		}
		seen[a.Name] = true
		out = append(out, a)
	}
	for _, a := range snap.Aliases {
		add(a)
		if r, ok := RepairCandidate(a); ok {
			add(r)
		}
	}
	return out
}

// BuildPlan reconciles a snapshot against probe verdicts keyed by alias name.
//
// For each snapshot alias the profile-derived spelling wins when it is keepable,
// because it is the only spelling cc cannot rewrite out from under the override;
// otherwise the snapshot's own spelling is used when keepable; otherwise the
// alias is dropped with the verdict that dropped it.
func BuildPlan(snap Snapshot, verdicts map[string]Verdict) Plan {
	var plan Plan
	for _, a := range snap.Aliases {
		if r, ok := RepairCandidate(a); ok {
			rv, probed := verdicts[r.Name]
			if keepable(rv, probed) {
				plan.Aliases = append(plan.Aliases, r)
				plan.Repaired = append(plan.Repaired, Repair{From: a.Name, To: r.Name})
				continue
			}
		}
		v, probed := verdicts[a.Name]
		if keepable(v, probed) {
			plan.Aliases = append(plan.Aliases, a)
			continue
		}
		v.Alias = a.Name
		plan.Excluded = append(plan.Excluded, v)
	}
	sortAliases(plan.Aliases)
	for _, a := range plan.Aliases {
		v, probed := verdicts[a.Name]
		if v.Window > 0 {
			if plan.Windows == nil {
				plan.Windows = map[string]int{}
			}
			plan.Windows[a.Name] = v.Window
		}
		if probed && a.OneM() && v.Window > 0 && v.Window < oneMWindow {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("%s advertises 1M but the probe turn ran with a %d-token window", a.Name, v.Window))
		}
	}
	return plan
}

// Available returns the availableModels value, in picker order.
func (p Plan) Available() []string {
	out := make([]string, 0, len(p.Aliases))
	for _, a := range p.Aliases {
		out = append(out, a.Name)
	}
	return out
}

// Overrides returns the modelOverrides value.
func (p Plan) Overrides() map[string]string {
	out := make(map[string]string, len(p.Aliases))
	for _, a := range p.Aliases {
		out[a.Name] = a.Profile
	}
	return out
}

// Empty reports whether the plan would offer no models at all, which callers
// MUST refuse to write: it would leave cc with an empty allowlist.
func (p Plan) Empty() bool { return len(p.Aliases) == 0 }
