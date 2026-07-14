package envpolicy

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Access-profile env overlay policy (RFC project-access-profile §5).
//
// An access profile is a NAMED set of env-var overrides layered onto a shim
// spawn per-session (e.g. "1P Anthropic direct" vs "company Bedrock"). This
// file is the single source of truth for WHICH keys a profile may set and WHAT
// values are legal for each. It deliberately exposes a SMALLER surface than the
// shim's full env allowlist (internal/shim shimEnvAllowedPrefixes):
//
//   - Only the auth/upstream selectors a profile legitimately switches are
//     allowed. AWS_PROFILE / AWS_*_FILE / AWS_ACCESS_KEY_ID and friends are NOT
//     overlay-settable — Bedrock account switching goes through a proxy port
//     (ANTHROPIC_BEDROCK_BASE_URL), never by handing the CLI a fresh AWS profile
//     (RFC §3 constraint; awsEnvDenyList already blocks the parent-injection
//     side).
//
// The overlay is NOT a whitelist bypass: after the session layer expands
// *_FILE references and merges the overlay onto the shim baseline, the result
// STILL passes through shim.filterShimEnv (exact-key allowlist + SSRF/profile
// value guards). Validation here is early feedback (config load, picker
// preflight) so an operator sees "unknown key" before a session is spawned.

// OverlayAllowedKeys is the exact set of env keys an access profile may set.
// A profile may only override these; anything else is a config error. This is a
// strict subset of the shim's env allowlist — see the package doc for why AWS
// profile/credential keys are intentionally excluded.
var OverlayAllowedKeys = map[string]bool{
	"ANTHROPIC_BASE_URL":            true,
	"ANTHROPIC_BEDROCK_BASE_URL":    true,
	"ANTHROPIC_MODEL":               true,
	"ANTHROPIC_AUTH_TOKEN":          true,
	"ANTHROPIC_API_KEY":             true,
	"CLAUDE_CODE_OAUTH_TOKEN":       true,
	"CLAUDE_CODE_USE_BEDROCK":       true,
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH": true,
	"AWS_REGION":                    true,
	"AWS_DEFAULT_REGION":            true,
}

// overlayFileKeys maps a "*_FILE" indirection key an access profile may set to
// the concrete secret key its file content is injected as. A profile stores the
// FILE path (safe to commit — it names a host file, not the secret) and the
// session layer reads the file and injects the resolved key at spawn time.
// R project-access-profile §4.3.
var overlayFileKeys = map[string]string{
	"ANTHROPIC_AUTH_TOKEN_FILE":    "ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY_FILE":       "ANTHROPIC_API_KEY",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE": "CLAUDE_CODE_OAUTH_TOKEN",
}

// ResolvedFileKey returns the concrete secret key a "*_FILE" overlay key
// expands into, and whether fileKey is a recognised indirection key. Callers in
// the session layer use this to know which non-FILE key to inject the file's
// contents as. Unknown keys return ("", false).
func ResolvedFileKey(fileKey string) (string, bool) {
	k, ok := overlayFileKeys[fileKey]
	return k, ok
}

// IsOverlayFileKey reports whether key is a "*_FILE" secret-indirection key an
// access profile is allowed to declare.
func IsOverlayFileKey(key string) bool {
	_, ok := overlayFileKeys[key]
	return ok
}

// ValidateOverlayEntry validates a single access-profile env entry (key/value)
// against the overlay policy. It enforces:
//
//   - key ∈ OverlayAllowedKeys, OR key is a recognised *_FILE indirection key;
//   - endpoint values (ANTHROPIC_*BASE_URL) pass the SSRF/redirect guard;
//   - AWS_REGION / AWS_DEFAULT_REGION pass the profile-name charset guard
//     (regions are a subset of that charset — no path separators / metachars);
//   - *_FILE values are absolute, traversal-free, null-free host paths.
//
// It does NOT stat the *_FILE path (that is a spawn-time / preflight I/O concern
// handled by the session layer) — this leaf is pure so config load can call it.
// Returns nil when the entry is legal.
func ValidateOverlayEntry(key, value string) error {
	if key == "" {
		return fmt.Errorf("empty env key")
	}
	if concrete, ok := overlayFileKeys[key]; ok {
		// A *_FILE key carries a host path, not the secret itself. The
		// concrete key it expands into must itself be overlay-settable
		// (defence-in-depth against a future map typo).
		if !OverlayAllowedKeys[concrete] {
			return fmt.Errorf("file key %q expands to non-overlay key %q", key, concrete)
		}
		if !isSafeOverlayFilePath(value) {
			return fmt.Errorf("key %q: value must be an absolute, traversal-free file path", key)
		}
		return nil
	}
	if !OverlayAllowedKeys[key] {
		return fmt.Errorf("key %q not permitted in access profile (not in overlay allowlist)", key)
	}
	switch key {
	case "ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_BASE_URL":
		if err := ValidateBaseURLValue(value); err != nil {
			return fmt.Errorf("key %q: %w", key, err)
		}
	case "AWS_REGION", "AWS_DEFAULT_REGION":
		// Regions (e.g. "us-west-2") are a strict subset of the safe
		// profile charset; reuse it to reject metachars / path separators.
		if value != "" && !IsSafeProfileValue(value) {
			return fmt.Errorf("key %q: value %q not a valid region token", key, value)
		}
	}
	return nil
}

// isSafeOverlayFilePath mirrors the shim's credential-path guard: non-empty,
// null-free, absolute, and free of any ".." segment. The file is READ by naozhi
// (not the CLI subprocess) but a tampered project.yaml must never point the read
// at an arbitrary host file, so the same traversal guard applies.
func isSafeOverlayFilePath(v string) bool {
	if v == "" || strings.IndexByte(v, 0) >= 0 || !filepath.IsAbs(v) {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(v), "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}
