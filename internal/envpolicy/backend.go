package envpolicy

import "strings"

// Per-backend raw-credential key sets. Only the detected backend's set passes
// through; every other backend's secrets are stripped even if present in the
// parent env, shrinking the blast radius of an inherited runner env (#1400).
var (
	// envCredsAnthropic — direct-Anthropic API auth.
	envCredsAnthropic = []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
	}
	// envCredsAWS — Bedrock static creds (empty on EC2 instance-role
	// deployments where IMDS supplies creds inside the SDK).
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
