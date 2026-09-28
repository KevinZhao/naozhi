package cli

import "sort"

// AllDeathReasons lists every DeathReason* constant's value (internal/cli
// process.go). Exported so contractjs can enumerate the wire vocabulary
// instead of restating it as JS string literals (#2909 G5 PR2).
// enum_wire_test.go's AST check keeps this in step with process.go: a new
// DeathReason* constant not added here fails that test.
func AllDeathReasons() []string {
	return []string{
		DeathReasonCLIExited,
		DeathReasonShimEOF,
		DeathReasonShimReadErr,
		DeathReasonShimOversizeThenEOF,
		DeathReasonShimOversizeThenErr,
		DeathReasonReadLoopPanic,
		DeathReasonKilled,
		DeathReasonNoOutputTimeout,
		DeathReasonTotalTimeout,
	}
}

// AllSessionStates lists the distinct wire values ProcessState.String() can
// return, deduped and sorted ("unknown" excluded: State's default branch,
// never produced by a value the const block declares).
func AllSessionStates() []string {
	seen := map[string]bool{}
	var out []string
	for s := StateSpawning; s <= StateDead; s++ {
		if v := s.String(); !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
