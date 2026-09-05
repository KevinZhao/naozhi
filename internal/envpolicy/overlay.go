package envpolicy

import "fmt"

// Access-profile env overlay policy (RFC project-access-profile §5): which
// keys a named per-session env override set may touch and what values are
// legal, per the Table's SourceOverlay column. Deliberately a SMALLER surface
// than the shim's: AWS_PROFILE / AWS_*_FILE / AWS_ACCESS_KEY_ID are NOT
// overlay-settable — Bedrock account switching goes through a proxy port
// (ANTHROPIC_BEDROCK_BASE_URL), never a fresh AWS profile handed to the CLI.
// Not a whitelist bypass: the merged env STILL passes FilterShimEnv;
// validation here is early feedback at config load / picker preflight.

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

// ValidateOverlayEntry validates a single access-profile env entry against
// the Table's SourceOverlay column: the key must be overlay-allowed, and the
// value must pass the key's overlay guard (SSRF/redirect for *BASE_URL,
// region charset for AWS_*_REGION, absolute traversal-free host path for the
// *_FILE indirection keys). Does NOT stat the file — this leaf stays pure.
func ValidateOverlayEntry(key, value string) error {
	if key == "" {
		return fmt.Errorf("empty env key")
	}
	rule, allowed := Allowed(key, SourceOverlay)
	if !allowed {
		return fmt.Errorf("key %q not permitted in access profile (not in overlay allowlist)", key)
	}
	if g, ok := rule.Guards[SourceOverlay]; ok {
		if err := g.check(value); err != nil {
			return fmt.Errorf("key %q: %w", key, err)
		}
	}
	return nil
}
