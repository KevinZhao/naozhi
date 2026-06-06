package envpolicy

import "strings"

// Per-backend raw-credential key sets. R040034-SEC-4 (#1400). Only the set
// matching the detected backend is layered onto the always-passthrough set by
// callers; every other backend's secrets are stripped even if present in the
// parent env. This shrinks the blast radius if a Claude subprocess Bash tool
// inherits the runner env.
var (
	// envCredsAnthropic — direct-Anthropic API auth.
	envCredsAnthropic = []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
	}
	// envCredsAWS — Bedrock static creds. On EC2 instance-role deployments
	// these are empty in the parent (IMDS supplies creds inside the SDK) so
	// the passthrough is a no-op there; they matter for non-EC2 Bedrock
	// setups that export static keys.
	envCredsAWS = []string{
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
	}
	// envCredsVertex — GCP service-account credential file path.
	envCredsVertex = []string{
		"GOOGLE_APPLICATION_CREDENTIALS",
	}
)

// BackendMode is the credential-gating dimension derived from the parent env's
// backend selectors. R040034-SEC-4 (#1400).
type BackendMode int

const (
	BackendAnthropic BackendMode = iota // direct Anthropic API (default)
	BackendBedrock                      // CLAUDE_CODE_USE_BEDROCK truthy
	BackendVertex                       // CLAUDE_CODE_USE_VERTEX truthy
)

// EnvTruthy reports whether a CLAUDE_CODE_USE_* selector value enables that
// backend. Matches the CLI's own loose truthiness (any non-empty,
// non-"0"/"false" value). We intentionally treat "0"/"false"/"" as off so an
// explicitly-disabled selector doesn't mis-route the gate.
func EnvTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// DetectBackendFromEnv inspects the parent env ("KEY=value" slice) for the
// CLAUDE_CODE_USE_* selectors. Bedrock wins over Vertex if both are somehow set
// (matches the CLI's precedence: it checks Bedrock first). Absence of both
// selectors means direct Anthropic.
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

// EnvCredsForBackend returns the raw-credential keys that may pass through for
// the given backend. Keys outside this set are stripped even when present in
// the parent env. R040034-SEC-4 (#1400).
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

// AllCredKeys is the union of every backend's raw-credential keys. Used by
// callers to build the per-call deny set for the inactive backends.
// R040034-SEC-4 (#1400).
var AllCredKeys = func() []string {
	keys := make([]string, 0, len(envCredsAnthropic)+len(envCredsAWS)+len(envCredsVertex))
	keys = append(keys, envCredsAnthropic...)
	keys = append(keys, envCredsAWS...)
	keys = append(keys, envCredsVertex...)
	return keys
}()
