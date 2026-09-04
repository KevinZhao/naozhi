package envpolicy

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Access-profile env overlay policy (RFC project-access-profile §5): the
// single source of truth for WHICH keys a named per-session env override set
// may touch and WHAT values are legal. Deliberately a SMALLER surface than the
// shim's env allowlist: AWS_PROFILE / AWS_*_FILE / AWS_ACCESS_KEY_ID are NOT
// overlay-settable — Bedrock account switching goes through a proxy port
// (ANTHROPIC_BEDROCK_BASE_URL), never a fresh AWS profile handed to the CLI.
// Not a whitelist bypass: the merged env STILL passes shim.filterShimEnv;
// validation here is early feedback at config load / picker preflight.

// OverlayAllowedKeys is the exact set of env keys an access profile may set.
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

// overlayFileKeys maps a "*_FILE" indirection key (a committable host path)
// to the concrete secret key the session layer injects at spawn time.
var overlayFileKeys = map[string]string{
	"ANTHROPIC_AUTH_TOKEN_FILE":    "ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY_FILE":       "ANTHROPIC_API_KEY",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE": "CLAUDE_CODE_OAUTH_TOKEN",
}

// ResolvedFileKey returns the concrete key a "*_FILE" overlay key expands into.
func ResolvedFileKey(fileKey string) (string, bool) {
	k, ok := overlayFileKeys[fileKey]
	return k, ok
}

// IsOverlayFileKey reports whether key is a recognised "*_FILE" indirection key.
func IsOverlayFileKey(key string) bool {
	_, ok := overlayFileKeys[key]
	return ok
}

// ValidateOverlayEntry validates a single access-profile env entry: key must
// be in OverlayAllowedKeys or a *_FILE indirection key; ANTHROPIC_*BASE_URL
// values pass the SSRF/redirect guard; AWS_REGION values pass the
// profile-name charset guard; *_FILE values are absolute, traversal-free,
// null-free host paths. Does NOT stat the file — this leaf stays pure.
func ValidateOverlayEntry(key, value string) error {
	if key == "" {
		return fmt.Errorf("empty env key")
	}
	if concrete, ok := overlayFileKeys[key]; ok {
		// Defence-in-depth against a future map typo.
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
		// Regions are a strict subset of the safe profile charset.
		if value != "" && !IsSafeProfileValue(value) {
			return fmt.Errorf("key %q: value %q not a valid region token", key, value)
		}
	}
	return nil
}

// isSafeOverlayFilePath mirrors the shim's credential-path guard so a tampered
// project.yaml cannot point naozhi's read at an arbitrary host file.
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
