package sysession

import (
	"os"
	"strings"

	"github.com/naozhi/naozhi/internal/envpolicy"
	"github.com/naozhi/naozhi/internal/spawndiag"
)

// A var the Runner env gate refuses is configured input that never reaches the
// daemon's `claude -p`.
const (
	sysessionEnvScope = "sysession-env"
	layerEnvFilter    = "env-filter"
)

// envAllowed reports whether the sysession column of the envpolicy Table passes
// key through to a Runner's `claude -p`. The three lists this used to keep —
// always-passthrough, profile-name keys, base-URL keys — are Table rows with the
// SourceSysession bit; table_sysession_test.go pins every one of them against a
// copy of the retired maps.
func envAllowed(key string) bool {
	_, ok := envpolicy.Allowed(key, envpolicy.SourceSysession)
	return ok
}

// envGuardFor returns the value check the Table attaches to key for this
// column, or nil. AWS profile names get the charset guard (a crafted name can
// point credential_process at an attacker binary, #1617); base URLs get
// ValidateBaseURLValue, so a tampered parent env cannot aim the CLI at an
// IMDS/internal http endpoint and tunnel an SSRF past the settings.json-side
// guard (#1687).
func envGuardFor(key string) func(string) error {
	return envpolicy.GuardFor(key, envpolicy.SourceSysession)
}

// backendMode aliases envpolicy.BackendMode (#891). Only the credential set of
// the detected backend is layered onto the sysession column; every other
// backend's secrets are stripped even if present in the parent env (#1400).
type backendMode = envpolicy.BackendMode

// detectBackendFromEnv delegates to envpolicy.DetectBackendFromEnv (#891).
func detectBackendFromEnv(parent []string) backendMode {
	return envpolicy.DetectBackendFromEnv(parent)
}

// envCredsForBackend delegates to envpolicy.EnvCredsForBackend (#891).
func envCredsForBackend(mode backendMode) []string {
	return envpolicy.EnvCredsForBackend(mode)
}

// filterEnv returns the exec.Cmd.Env slice for a Runner: the keys the Table's
// sysession column passes, the raw-credential keys of the *detected* backend only, allowlist
// exact matches, and prefix matches for allowlist entries ending in "_"
// ("ANTHROPIC_" matches every ANTHROPIC_* var; "ANTHROPIC" only the bare key).
// Credentials of NON-active backends are stripped unconditionally — even when
// a broad prefix such as "ANTHROPIC_" / "AWS_" would re-admit them — so a
// Bedrock-only deployment never hands ANTHROPIC_API_KEY (or Vertex's
// GOOGLE_APPLICATION_CREDENTIALS) to CLI tool subprocesses where prompt
// content could exfiltrate them (#1400). Everything else in the parent env is
// dropped. Matching is case-sensitive; a nil allowlist is fine.
func filterEnv(allowlist []string) []string {
	parent := os.Environ()

	mode := detectBackendFromEnv(parent)
	allowedCreds := make(map[string]struct{}, len(envCredsForBackend(mode)))
	for _, k := range envCredsForBackend(mode) {
		allowedCreds[k] = struct{}{}
	}
	// Inactive-backend credentials are dropped regardless of allowlist so a
	// broad prefix entry can't tunnel a sibling-backend secret through.
	blockedCreds := make(map[string]struct{}, len(allCredKeys))
	for _, k := range allCredKeys {
		if _, ok := allowedCreds[k]; !ok {
			blockedCreds[k] = struct{}{}
		}
	}

	exact := make(map[string]struct{}, len(allowlist))
	var prefixes []string
	for _, k := range allowlist {
		if strings.HasSuffix(k, "_") {
			prefixes = append(prefixes, k)
			continue
		}
		exact[k] = struct{}{}
	}

	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		// Split on first '=' only; values may contain '='.
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		key := kv[:idx]
		// Hard gate: a non-active-backend credential is never emitted, even if
		// the allowlist or always-set would admit it.
		if _, blocked := blockedCreds[key]; blocked {
			continue
		}
		if _, ok := allowedCreds[key]; ok {
			out = append(out, kv)
			continue
		}
		if envAllowed(key) {
			// The value guard is a gate: an operator exported this var for the
			// daemon and it will not arrive, so report it rather than log it.
			if check := envGuardFor(key); check != nil {
				if err := check(kv[idx+1:]); err != nil {
					// The value itself is never reported — it may be
					// credential-adjacent — and GuardErrorReason collapses the
					// copy of it that url.Parse errors carry.
					spawndiag.One(sysessionEnvScope, layerEnvFilter, key, "dropped",
						"value fails its guard: "+envpolicy.GuardErrorReason(err))
					continue
				}
			}
			out = append(out, kv)
			continue
		}
		if _, ok := exact[key]; ok {
			out = append(out, kv)
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(key, p) {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

// allCredKeys is the union of every backend's raw-credential keys; filterEnv
// builds the inactive-backend deny set from it (#891).
var allCredKeys = envpolicy.AllCredKeys
