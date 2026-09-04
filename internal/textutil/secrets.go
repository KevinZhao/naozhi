// secrets.go: well-known token-prefix scrubbing for any text that may echo
// plaintext credentials before persistence / broadcast / IM reply (#1571).
//
//   - Token-wise scan, not regex: every cron tick and IM reply flows through
//     here, so a byte scan over a small prefix table beats a regex.
//   - Prefix list is conservative — only unambiguous markers (`sk-ant-`,
//     `ghp_`, `AKIA`); generic password-like patterns are out of scope because
//     false positives corrupt operator diagnostics.
//   - Idempotent: `[REDACTED]` starts with no registered marker.
//   - Empty / no-prefix inputs return the aliased input without allocation.

package textutil

import (
	"regexp"
	"strings"
)

// secretPrefix names a well-known token prefix RedactSecrets recognises.
// minTail is a sanity floor: a shorter post-prefix run is treated as a
// literal substring (avoids redacting "ghp_" in prose / a doc URL).
type secretPrefix struct {
	prefix  string
	minTail int
}

// secretPrefixes are the patterns RedactSecrets scans for. Longer members of
// a family (`sk-ant-`, `sk-proj-`) must precede the bare fallback (`sk-`) so
// the longest match wins. Covers Anthropic, OpenAI, GitHub, GitLab, AWS,
// Slack, HuggingFace, npm, Google OAuth, Databricks, Vault, Stripe and PEM
// headers, following common secret-scanner conventions (#1006).
var secretPrefixes = []secretPrefix{
	{prefix: "sk-ant-", minTail: 8},
	{prefix: "sk-proj-", minTail: 16},
	{prefix: "sk-", minTail: 40},
	{prefix: "npm_", minTail: 16},
	{prefix: "ghp_", minTail: 16},
	{prefix: "gho_", minTail: 16},
	{prefix: "ghu_", minTail: 16},
	{prefix: "ghs_", minTail: 16},
	{prefix: "ghr_", minTail: 16},
	{prefix: "github_pat_", minTail: 16},
	{prefix: "glpat-", minTail: 16},
	{prefix: "AKIA", minTail: 16},
	{prefix: "ASIA", minTail: 16},
	{prefix: "xoxb-", minTail: 16},
	{prefix: "xoxp-", minTail: 16},
	{prefix: "xoxa-", minTail: 16},
	{prefix: "xoxs-", minTail: 16},
	{prefix: "hf_", minTail: 16},
	{prefix: "ya29.", minTail: 16},
	// Databricks personal-access tokens: 32 hex chars after the prefix.
	{prefix: "dapi", minTail: 16},
	// HCP Vault service tokens. The `.` is part of the prefix; the base64url
	// body may contain `-` and `_`, which isSecretTokenByte handles.
	{prefix: "hvs.", minTail: 16},
	// Stripe live/test secret keys.
	{prefix: "sk_live_", minTail: 16},
	{prefix: "sk_test_", minTail: 16},
	// Stripe restricted keys: scoped, but still able to call the Stripe API.
	{prefix: "rk_live_", minTail: 16},
	{prefix: "rk_test_", minTail: 16},
	// PEM headers. The tail scan stops at the first space, so minTail=0: the
	// `-----BEGIN` token alone is unambiguous; the rest of the header line and
	// the base64 body are not redacted by this token scanner. `eyJ` (JWT) is
	// deliberately absent: it is base64(`{"`) and would false-positive on any
	// base64 JSON, and JWT dots would end the tail scan immediately anyway.
	{prefix: "-----BEGIN", minTail: 0},
}

// secretPrefixesByFirstByte indexes secretPrefixes by first byte so the
// RedactSecrets inner loop probes only the 1-3 candidates that can match
// (#1976). Buckets keep declaration order so longest-match-wins holds.
// Built once at init and never mutated, so concurrent reads are safe.
var secretPrefixesByFirstByte = buildSecretPrefixIndex()

func buildSecretPrefixIndex() map[byte][]secretPrefix {
	idx := make(map[byte][]secretPrefix)
	for _, sp := range secretPrefixes {
		if sp.prefix == "" {
			continue
		}
		first := sp.prefix[0]
		idx[first] = append(idx[first], sp)
	}
	return idx
}

// secretRedactedMarker replaces matched secret bytes. Distinct from
// `…[truncated]` so dashboard / SIEM filters can tell redaction from truncation.
const secretRedactedMarker = "[REDACTED]"

// envAssignmentRe matches `KEY=value` (and `KEY = value`) assignments whose
// KEY carries a credential marker, so RedactSecrets masks values that are not
// a known token prefix (`MY_CUSTOM_SECRET=hunter2`) (#2165). Only the value
// is masked. SECRET/PASSWORD/PASSWD/CREDENTIAL match as an infix
// (`SECRET_KEY_BASE`); TOKEN / AUTH / *_KEY are suffix-anchored to avoid
// `TOKENIZER`, `AUTHOR`, `KEYBOARD`. Values may be double-/single-quoted,
// JSON-escaped (`\"…\"`), quote-opened-but-unterminated, or a bare run; the
// bare run consumes backslash-escaped bytes as pairs and stops before an
// unescaped or escaped quote so an assignment inside a JSON string does not
// swallow the closing `"}` and break the document (#2439). Idempotent.
var envAssignmentRe = regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:(?:SECRET|PASSWORD|PASSWD|CREDENTIAL)[A-Z0-9_]*|TOKEN|API_?KEY|ACCESS_?KEY|PRIVATE_?KEY|AUTH))\s*=\s*("[^"]*"|'[^']*'|\\"(?:[^"\\]|\\.)*\\"|["'](?:[^\s"'\\]|\\[^"'\s])+|(?:[^\s"'\\]|\\[^"'\s])+)`)

// RedactSecrets walks s once, swapping any well-known secret-prefix pattern
// for `[REDACTED]`. Returns the original (aliased) string when nothing
// matched, so clean output pays only the byte scan. Idempotent.
func RedactSecrets(s string) string {
	if s == "" {
		return s
	}
	// KEY=value masking runs first so a sensitive value that is NOT a known
	// token prefix (`MY_SECRET=hunter2`) is still scrubbed; gated on a cheap
	// IndexByte('=') so the common no-`=` path pays nothing (#2165).
	s = redactEnvAssignments(s)
	// Most output contains no prefix first byte: alias the input, no allocation.
	if !mayContainSecretPrefix(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		matched := false
		// Only prefixes whose first byte equals s[i] can match; a byte with no
		// registered prefix skips the inner loop entirely (#1976).
		for _, sp := range secretPrefixesByFirstByte[s[i]] {
			if !strings.HasPrefix(s[i:], sp.prefix) {
				continue
			}
			tailStart := i + len(sp.prefix)
			tailEnd := tailStart
			for tailEnd < len(s) && isSecretTokenByte(s[tailEnd]) {
				tailEnd++
			}
			if tailEnd-tailStart < sp.minTail {
				// Not a secret — fall through to literal copy below.
				continue
			}
			b.WriteString(secretRedactedMarker)
			i = tailEnd
			matched = true
			break
		}
		if !matched {
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// isSecretTokenByte reports whether b continues a secret tail: alphanumerics
// plus `-` and `_`; anything else terminates the run.
func isSecretTokenByte(b byte) bool {
	switch {
	case b >= '0' && b <= '9':
		return true
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b == '-' || b == '_':
		return true
	default:
		return false
	}
}

// mayContainSecretPrefix is a fast pre-scan: false if no first byte of any
// registered prefix appears in s. First-byte set: 's' 'g' 'A' 'x' 'h' 'n'
// 'y' 'd' 'r' '-' — keep in sync with secretPrefixes.
func mayContainSecretPrefix(s string) bool {
	return strings.ContainsAny(s, "sgAxhnydr-")
}

// redactEnvAssignments masks the value of any credential-named `KEY=value`
// assignment (see envAssignmentRe). Aliases the input when no `=` is present
// so the hot path stays zero-alloc. Idempotent (#2165).
func redactEnvAssignments(s string) string {
	if strings.IndexByte(s, '=') < 0 {
		return s
	}
	return envAssignmentRe.ReplaceAllStringFunc(s, func(m string) string {
		idx := strings.IndexByte(m, '=')
		if idx < 0 {
			return m
		}
		// Keep `KEY` + spaces + `=` + spaces so diagnostics retain the key name.
		valStart := idx + 1
		for valStart < len(m) && (m[valStart] == ' ' || m[valStart] == '\t') {
			valStart++
		}
		val := m[valStart:]
		if len(val) >= 4 && strings.HasPrefix(val, `\"`) && strings.HasSuffix(val, `\"`) {
			// JSON-escaped quoted span: keep both escaped delimiters so the
			// enclosing JSON stays well-formed.
			return m[:valStart] + `\"` + secretRedactedMarker + `\"`
		}
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			// Quoted span: keep both quotes, mask the whole span (may contain spaces).
			q := string(val[0])
			return m[:valStart] + q + secretRedactedMarker + q
		}
		if len(val) >= 1 && (val[0] == '"' || val[0] == '\'') {
			// Quote-opened but unterminated (e.g. a multiline PEM dump): keep the
			// leading quote so the `-----BEGIN` scanner still sees the line.
			return m[:valStart+1] + secretRedactedMarker
		}
		return m[:valStart] + secretRedactedMarker
	})
}
