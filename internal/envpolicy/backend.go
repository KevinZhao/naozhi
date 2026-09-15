package envpolicy

import "strings"

// Per-backend raw-credential key sets, derived from the Table's Cred marks so
// "which key is which backend's credential" is stated once, on the row that also
// carries that key's per-source verdicts. Only the detected backend's set passes
// through; every other backend's secrets are stripped even if present in the
// parent env, shrinking the blast radius of an inherited runner env (#1400).
var (
	// envCredsAnthropic — direct-Anthropic API auth.
	envCredsAnthropic = credKeys(credAnthropic)
	// envCredsAWS — Bedrock static creds (empty on EC2 instance-role
	// deployments where IMDS supplies creds inside the SDK).
	envCredsAWS = credKeys(credAWS)
	// envCredsVertex — GCP service-account credential file path.
	envCredsVertex = credKeys(credVertex)
)

// credKeys collects the exact-match Table patterns marked with class, in Table
// order. A wildcard pattern would make "the key set" unenumerable, so it is a
// programming error here rather than a silently skipped row.
func credKeys(class credClass) []string {
	var out []string
	for _, r := range Table {
		if r.Cred != class {
			continue
		}
		if strings.ContainsRune(r.Pattern, '*') {
			panic("envpolicy: credential rule " + r.Pattern + " must be an exact key")
		}
		out = append(out, r.Pattern)
	}
	return out
}

// BackendMode is the credential-gating dimension derived from CLAUDE_CODE_USE_*.
type BackendMode int

const (
	BackendAnthropic BackendMode = iota // direct Anthropic API (default)
	BackendBedrock                      // CLAUDE_CODE_USE_BEDROCK truthy
	BackendVertex                       // CLAUDE_CODE_USE_VERTEX truthy
)

// EnvTruthy reports whether a CLAUDE_CODE_USE_* selector value enables that
// backend (CLI's loose truthiness; "0"/"false"/"" are off).
func EnvTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// DetectBackendFromEnv inspects the parent env ("KEY=value" slice) for the
// CLAUDE_CODE_USE_* selectors. Bedrock wins over Vertex (CLI precedence).
func DetectBackendFromEnv(parent []string) BackendMode {
	var bedrock, vertex string
	for _, kv := range parent {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CODE_USE_BEDROCK="); ok {
			bedrock = v
		} else if v, ok := strings.CutPrefix(kv, "CLAUDE_CODE_USE_VERTEX="); ok {
			vertex = v
		}
	}
	switch {
	case EnvTruthy(bedrock):
		return BackendBedrock
	case EnvTruthy(vertex):
		return BackendVertex
	default:
		return BackendAnthropic
	}
}

// EnvCredsForBackend returns the raw-credential keys that may pass through.
func EnvCredsForBackend(mode BackendMode) []string {
	switch mode {
	case BackendBedrock:
		return envCredsAWS
	case BackendVertex:
		return envCredsVertex
	default:
		return envCredsAnthropic
	}
}

// AllCredKeys is the union of every backend's raw-credential keys (deny set
// for the inactive backends).
var AllCredKeys = func() []string {
	keys := make([]string, 0, len(envCredsAnthropic)+len(envCredsAWS)+len(envCredsVertex))
	keys = append(keys, envCredsAnthropic...)
	keys = append(keys, envCredsAWS...)
	keys = append(keys, envCredsVertex...)
	return keys
}()
