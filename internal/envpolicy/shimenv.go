package envpolicy

// Shim-side env filtering: which inherited variables reach shim/CLI
// subprocesses, and the value guards applied to the sensitive ones. Key-level
// allowance comes from Table (SourceShim); the CONTRACT from the shim days
// still holds — do NOT widen the shim column to make a settings.json value
// "take effect": the spawned claude reads ~/.claude/settings.json itself and
// a settings.json `env` value WINS over the inherited process env, so
// forwarding functional knobs is redundant and re-widens the leak surface.
// Only system/toolchain plumbing and raw Bedrock credentials that
// settings.json does NOT carry belong in the shim column.

import (
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// maxShimEnvEntryBytes caps a single forwarded env entry. Legitimate allowlisted
// values are well under 4 KiB; a pathological one only inflates the child env
// and slog attrs, so reject and log instead.
const maxShimEnvEntryBytes = 4 * 1024

// maxShimEnvOversizeWarnings caps oversized-entry warnings per process
// lifetime. A counter (not sync.Once) so one benign oversized entry cannot
// mask a later attacker-injected one while log volume stays bounded.
const maxShimEnvOversizeWarnings = 5

// filterShimEnvOversizeWarnings counts emitted oversized-entry warnings;
// entries are always rejected, only the logging is capped.
var filterShimEnvOversizeWarnings atomic.Int64

// FilterShimEnv returns a copy of environ keeping only variables whose key
// the Table allows for SourceShim (defense-in-depth against `env` via the
// Bash tool). Oversized entries are rejected and logged by key prefix only.
func FilterShimEnv(environ []string) []string {
	filtered := make([]string, 0, len(environ)/2)
	for _, kv := range environ {
		if len(kv) > maxShimEnvEntryBytes {
			// Key prefix only — never the value (may be a secret). Logging is
			// capped at maxShimEnvOversizeWarnings; rejection is not.
			if n := filterShimEnvOversizeWarnings.Add(1); n <= maxShimEnvOversizeWarnings {
				msg := "shim env: oversized entry rejected"
				if n == maxShimEnvOversizeWarnings {
					msg = "shim env: oversized entry rejected (further oversized warnings suppressed)"
				}
				slog.Warn(msg,
					"key_prefix", kvKeyPrefix(kv),
					"len", len(kv),
					"max", maxShimEnvEntryBytes)
			}
			continue
		}
		if !shimKeyAllowed(kv) {
			continue
		}
		// Endpoint vars steer where the CLI (Bash + raw network) sends API
		// traffic; a poisoned rc pointing one at an attacker host or IMDS over
		// plain http would silently redirect/harvest. https for non-loopback (#1576).
		if shimEndpointEnvDropped(kv) {
			continue
		}
		// AWS_PROFILE / AWS_DEFAULT_PROFILE select a profile that may declare a
		// credential_process the SDK executes; restrict to ^[A-Za-z0-9_-]{1,64}$
		// (mirrors sysession/env.go isSafeProfileValue). Key logged, never value.
		if shimProfileEnvDropped(kv) {
			continue
		}
		// AWS_*_FILE vars name files the SDK opens in the CLI subprocess; a
		// value like /proc/self/environ or ../ traversal would ship arbitrary
		// host files to STS. Require an absolute, traversal-free, null-free path.
		if shimCredPathEnvDropped(kv) {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}

// shimKeyAllowed reports whether the Table's shim column forwards kv. An
// exact-key rule only matches a well-formed "KEY=value" entry (the historical
// allowlist stored exact keys as "KEY=" prefixes); a namespace rule
// ("LC_*") matches on the raw string, '=' or not, exactly like the
// historical raw-prefix match did.
func shimKeyAllowed(kv string) bool {
	key := kv
	hasEq := false
	if i := strings.IndexByte(kv, '='); i >= 0 {
		key, hasEq = kv[:i], true
	}
	rule, allowed := Allowed(key, SourceShim)
	if !allowed {
		return false
	}
	if !strings.HasSuffix(rule.Pattern, "*") && !hasEq {
		return false
	}
	return true
}

// MergeShimEnv layers a per-spawn env overlay (the materialised access profile,
// RFC project-access-profile §4: resolved "KEY=value" pairs) onto the process
// baseline and returns the effective env for one CLI subprocess.
//
// INVARIANT — the overlay is NOT a whitelist bypass: the merged slice is re-run
// through FilterShimEnv, so every overlay entry faces the same allowlist +
// SSRF/profile/cred-path guards. It may only override the VALUE of an already
// allowlisted key, per spawn. nil/empty overlay returns baseline unchanged.
// Overlay wins on conflict; ordering is baseline order then sorted extras so
// the argv-shape tests stay stable.
func MergeShimEnv(baseline []string, overlay map[string]string) []string {
	if len(overlay) == 0 {
		return baseline
	}
	merged := make([]string, 0, len(baseline)+len(overlay))
	usedOverlay := make(map[string]bool, len(overlay))
	for _, kv := range baseline {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if v, ok := overlay[key]; ok {
			merged = append(merged, key+"="+v)
			usedOverlay[key] = true
			continue
		}
		merged = append(merged, kv)
	}
	// Append overlay keys that had no baseline counterpart, in sorted order for
	// determinism. These still face FilterShimEnv below.
	extra := make([]string, 0, len(overlay))
	for k := range overlay {
		if !usedOverlay[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		merged = append(merged, k+"="+overlay[k])
	}
	// Re-gate: overlay values face the identical allowlist + guards. Zero bypass.
	return FilterShimEnv(merged)
}

// shimProfileEnvDropped reports whether kv ("KEY=value") is an AWS profile-name
// var whose value falls outside ^[A-Za-z0-9_-]{1,64}$ and must be dropped.
// Non-profile keys return false. The value is never logged.
func shimProfileEnvDropped(kv string) bool {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return false
	}
	key, val := kv[:i], kv[i+1:]
	if guardKindFor(key, SourceShim) != guardProfile {
		return false
	}
	if !IsSafeProfileValue(val) {
		slog.Warn("shim env: rejecting unsafe AWS profile value (credential_process injection guard)", "key", key)
		return true
	}
	return false
}

// shimCredPathEnvDropped reports whether kv ("KEY=value") is an AWS
// credential-file path var whose value is not absolute, traversal-free and
// null-free, and must be dropped. Non-path keys and safe values return false.
// The value is never logged.
func shimCredPathEnvDropped(kv string) bool {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return false
	}
	key, val := kv[:i], kv[i+1:]
	if guardKindFor(key, SourceShim) != guardCredPath {
		return false
	}
	if !isSafeCredFilePath(val) {
		slog.Warn("shim env: rejecting unsafe AWS credential file path (path traversal guard)", "key", key)
		return true
	}
	return false
}

// isSafeCredFilePath reports whether v is a safe absolute credential file
// path: non-empty, no embedded null byte, absolute, and no ".." segment (so
// /a/../../etc/shadow is rejected even though it begins with a slash). Shared
// by the shim's AWS_*_FILE guard and the overlay's *_FILE indirection guard.
func isSafeCredFilePath(v string) bool {
	if v == "" {
		return false
	}
	if strings.IndexByte(v, 0) >= 0 {
		return false
	}
	if !filepath.IsAbs(v) {
		return false
	}
	// Reject any path containing a ".." segment outright (even if it would
	// clean away) so a tampered value can never escape its intended root.
	for _, seg := range strings.Split(filepath.ToSlash(v), "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// shimEndpointEnvDropped reports whether kv ("KEY=value") is an endpoint URL
// var whose value targets a plain-http non-loopback host (or an internal IP)
// and must be dropped. Non-endpoint keys and safe URLs return false. The value
// is never logged (#1576).
func shimEndpointEnvDropped(kv string) bool {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return false
	}
	key, val := kv[:i], kv[i+1:]
	if guardKindFor(key, SourceShim) != guardShimEndpoint {
		return false
	}
	if val == "" {
		return false
	}
	if err := validateShimEndpointURL(val); err != nil {
		slog.Warn("shim env: rejecting unsafe endpoint base_url", "key", key, "err", err)
		return true
	}
	return false
}

// validateShimEndpointURL enforces https:// unless the host is loopback
// (localhost / 127.0.0.0/8 / ::1), where plain http is allowed for local mocks.
// Even https:// must not target a literal internal IP (loopback excepted):
// ANTHROPIC_BASE_URL=https://169.254.169.254/... would steer the CLI's client,
// API key in hand, at IMDS or an internal admin port (#1713). Only literal IPs
// are inspected — no DNS resolution here; hostname rebinding is out of scope.
//
// Shares ClassifyHost with ValidateBaseURLValue (#2300) so the range
// classification cannot drift, but is deliberately stricter: no
// NAOZHI_ALLOW_PRIVATE_BASE_URL escape hatch, and 0.0.0.0 / :: are rejected.
func validateShimEndpointURL(v string) error {
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		host := u.Hostname()
		// Deny-set = every internal class except loopback (local https
		// mocks). The classes are disjoint (pinned in envpolicy tests), so
		// clearing the loopback bit never un-denies another range.
		if k, ok := ClassifyHost(host); ok && k&^IPLoopback != 0 {
			return fmt.Errorf("https:// to internal IP %q rejected (SSRF/IMDS guard)", host)
		}
		return nil
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if k, ok := ClassifyHost(host); ok && k.Has(IPLoopback) {
			return nil
		}
		return fmt.Errorf("plain http:// to non-loopback host %q rejected (SSRF/redirect guard); use https://", host)
	}
	return fmt.Errorf("scheme %q not allowed; use https://", u.Scheme)
}

// kvKeyPrefix returns the key part (before '=') of a KEY=value env string,
// capped at 64 bytes to bound log line length even for pathologically long
// key names. Never returns the value.
func kvKeyPrefix(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		k := kv[:i]
		if len(k) > 64 {
			k = k[:64]
		}
		return k
	}
	// Malformed (no '='): return a safe prefix.
	if len(kv) > 64 {
		return kv[:64]
	}
	return kv
}

// Guard-check adapters referenced by Table.
func errIfUnsafeProfile(v string) error {
	if !IsSafeProfileValue(v) {
		return fmt.Errorf("value not a valid profile token")
	}
	return nil
}

func errIfUnsafeCredPath(v string) error {
	if !isSafeCredFilePath(v) {
		return fmt.Errorf("value must be an absolute, traversal-free file path")
	}
	return nil
}

func errIfUnsafeRegion(v string) error {
	if v != "" && !IsSafeProfileValue(v) {
		return fmt.Errorf("value %q not a valid region token", v)
	}
	return nil
}
