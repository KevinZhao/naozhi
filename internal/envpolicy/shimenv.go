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
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// Drop is one entry the shim filter refused for a GATE reason: the operator set
// it and it will not reach the child. A key the Table simply does not forward is
// not a Drop — that is everyday noise, not a rejection of configured input.
// Callers report Drops through internal/spawndiag; this package stays a leaf
// (TestPackageIsLeaf) so the classifier has no side channel of its own.
type Drop struct {
	// Key is the variable name, or a 64-byte key prefix when the entry was too
	// large to trust. The value is never included: it may be a secret.
	Key string
	// Reason is one human-readable sentence, safe to log.
	Reason string
}

// maxShimEnvEntryBytes caps a single forwarded env entry. Legitimate allowlisted
// values are well under 4 KiB; a pathological one only inflates the child env
// and slog attrs, so reject and log instead.
const maxShimEnvEntryBytes = 4 * 1024

// maxShimEnvOversizeReports caps oversized-entry reports per process lifetime.
// A counter (not sync.Once) so one benign oversized entry cannot mask a later
// attacker-injected one while log volume stays bounded.
const maxShimEnvOversizeReports = 5

// shimEnvOversizeReports counts reported oversized entries; entries are always
// rejected, only the reporting is capped.
var shimEnvOversizeReports atomic.Int64

// FilterShimEnv returns a copy of environ keeping only variables whose key
// the Table allows for SourceShim (defense-in-depth against `env` via the
// Bash tool) and whose value passes the key's guard. Pure: what it refused is
// reported by ShimEnvDrops, which walks the same verdict.
func FilterShimEnv(environ []string) []string {
	filtered := make([]string, 0, len(environ)/2)
	for _, kv := range environ {
		if keep, _ := shimEnvVerdict(kv); keep {
			filtered = append(filtered, kv)
		}
	}
	return filtered
}

// ShimEnvDrops reports the gate rejections FilterShimEnv makes for environ.
// Oversized entries are reported at most maxShimEnvOversizeReports times per
// process: an env carrying many distinct oversized keys would otherwise be a
// log-flooding amplifier. Rejection itself is never capped.
func ShimEnvDrops(environ []string) []Drop {
	var drops []Drop
	for _, kv := range environ {
		keep, reason := shimEnvVerdict(kv)
		if keep || reason == "" {
			continue
		}
		if len(kv) > maxShimEnvEntryBytes {
			n := shimEnvOversizeReports.Add(1)
			if n > maxShimEnvOversizeReports {
				continue
			}
			if n == maxShimEnvOversizeReports {
				reason += " (further oversized reports suppressed)"
			}
		}
		drops = append(drops, Drop{Key: kvKeyPrefix(kv), Reason: reason})
	}
	return drops
}

// shimEnvVerdict classifies one "KEY=value" entry: the single place the shim
// env decision is made, so the filter and its diagnostics cannot disagree (the
// rule extraArgsOverCap follows on the argv side). keep=false with an empty
// reason is the silent case — a key the shim column does not carry.
func shimEnvVerdict(kv string) (keep bool, reason string) {
	if len(kv) > maxShimEnvEntryBytes {
		return false, fmt.Sprintf("entry is %d bytes, over the %d-byte shim env cap", len(kv), maxShimEnvEntryBytes)
	}
	if !shimKeyAllowed(kv) {
		return false, ""
	}
	// Endpoint vars steer where the CLI (Bash + raw network) sends API
	// traffic; a poisoned rc pointing one at an attacker host or IMDS over
	// plain http would silently redirect/harvest. https for non-loopback (#1576).
	if reason := shimEndpointEnvDropped(kv); reason != "" {
		return false, reason
	}
	// AWS_PROFILE / AWS_DEFAULT_PROFILE select a profile that may declare a
	// credential_process the SDK executes; restrict to ^[A-Za-z0-9_-]{1,64}$
	// (mirrors sysession/env.go isSafeProfileValue).
	if reason := shimProfileEnvDropped(kv); reason != "" {
		return false, reason
	}
	// AWS_*_FILE vars name files the SDK opens in the CLI subprocess; a
	// value like /proc/self/environ or ../ traversal would ship arbitrary
	// host files to STS. Require an absolute, traversal-free, null-free path.
	if reason := shimCredPathEnvDropped(kv); reason != "" {
		return false, reason
	}
	return true, ""
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
	// Re-gate: overlay values face the identical allowlist + guards. Zero bypass.
	return FilterShimEnv(mergeShimEnvRaw(baseline, overlay))
}

// MergeShimEnvDrops reports the gate rejections MergeShimEnv makes — in
// practice the overlay entries, since the baseline was already filtered. Same
// merge, same verdict; the two cannot disagree.
func MergeShimEnvDrops(baseline []string, overlay map[string]string) []Drop {
	if len(overlay) == 0 {
		return nil
	}
	return ShimEnvDrops(mergeShimEnvRaw(baseline, overlay))
}

// mergeShimEnvRaw layers overlay onto baseline WITHOUT the re-gate: the shared
// half of MergeShimEnv and MergeShimEnvDrops. Never returned to a caller
// directly — an unfiltered env must not reach a subprocess.
func mergeShimEnvRaw(baseline []string, overlay map[string]string) []string {
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
	return merged
}

// shimProfileEnvDropped returns why kv ("KEY=value") must be dropped when it is
// an AWS profile-name var whose value falls outside ^[A-Za-z0-9_-]{1,64}$.
// Non-profile keys and safe values return "". The value is never echoed.
func shimProfileEnvDropped(kv string) (reason string) {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return ""
	}
	key, val := kv[:i], kv[i+1:]
	if guardKindFor(key, SourceShim) != guardProfile {
		return ""
	}
	if !IsSafeProfileValue(val) {
		return "AWS profile value is unsafe (credential_process injection guard)"
	}
	return ""
}

// shimCredPathEnvDropped returns why kv ("KEY=value") must be dropped when it
// is an AWS credential-file path var whose value is not absolute, traversal-free
// and null-free. Non-path keys and safe values return "". The value is never
// echoed.
func shimCredPathEnvDropped(kv string) (reason string) {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return ""
	}
	key, val := kv[:i], kv[i+1:]
	if guardKindFor(key, SourceShim) != guardCredPath {
		return ""
	}
	if !isSafeCredFilePath(val) {
		return "AWS credential file path is not absolute and traversal-free (path traversal guard)"
	}
	return ""
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

// shimEndpointEnvDropped returns why kv ("KEY=value") must be dropped when it
// is an endpoint URL
// var whose value targets a plain-http non-loopback host (or an internal IP)
// and must be dropped. Non-endpoint keys and safe URLs return "". The value is
// never echoed (#1576).
func shimEndpointEnvDropped(kv string) (reason string) {
	i := strings.IndexByte(kv, '=')
	if i < 0 {
		return ""
	}
	key, val := kv[:i], kv[i+1:]
	if guardKindFor(key, SourceShim) != guardShimEndpoint {
		return ""
	}
	if val == "" {
		return ""
	}
	if err := validateShimEndpointURL(val); err != nil {
		return fmt.Sprintf("endpoint base_url is unsafe: %v", err)
	}
	return ""
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
