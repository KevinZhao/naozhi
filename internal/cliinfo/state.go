package cliinfo

import "sort"

// ProcessState represents the lifecycle state of a CLI process.
type ProcessState int

const (
	StateSpawning ProcessState = iota
	StateReady
	StateRunning
	StateDead
)

func (s ProcessState) String() string {
	switch s {
	case StateSpawning:
		return "running" // spawning is transient; visible as running
	case StateReady:
		return "ready"
	case StateRunning:
		return "running"
	case StateDead:
		// Not "ready": the dashboard must not show crashed processes as idle.
		return "dead"
	default:
		return "unknown"
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
