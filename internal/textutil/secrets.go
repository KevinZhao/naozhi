// Package textutil — secrets.go: well-known token-prefix scrubbing for any
// text that may echo plaintext credentials before persistence / broadcast /
// IM reply.
//
// R20260602-091302-ARCH-1 (#1571): this redactor began life in
// internal/cron (R234-SEC-7, #1006) to scrub CronRun.Result before disk +
// WS broadcast. IM dispatch then started importing cron.RedactSecrets to
// scrub Claude replies (R103901-CODE-1) — a security-critical path coupled
// to a domain-unrelated package. The scan logic has zero cron semantics, so
// it now lives in this leaf package; cron keeps a thin alias and dispatch
// (and any future consumer: sysession / scratch / WS send_ack) imports
// textutil directly.
//
// Design notes (preserved from the original cron implementation):
//
//   - Token-wise scan, NOT regex. Every cron tick + every IM reply flows
//     through this path — a regex compile per call (or sync.Once'd globals)
//     costs more than a direct byte scan over the small prefix table.
//   - Prefix list is conservative: only patterns whose first 4-7 bytes are
//     unambiguous secret markers (`sk-ant-`, `ghp_`, `AKIA`). Generic
//     password-like patterns are out of scope — false positives on
//     legitimate Claude output corrupt operator diagnostics more than the
//     rare leak avoids.
//   - Idempotent: a second pass over an already-redacted string finds no
//     prefix because `[REDACTED]` does not start with any registered marker.
//   - Empty / no-prefix inputs return the aliased input without any
//     allocation.

package textutil

import (
	"regexp"
	"strings"
)

// secretPrefix names a well-known token prefix RedactSecrets recognises.
// minTail is a sanity floor: if the post-prefix byte run is shorter than
// minTail the prefix is treated as a literal substring rather than a secret
// (avoids redacting "ghp_" appearing in prose / a doc URL).
type secretPrefix struct {
	prefix  string
	minTail int
}

// secretPrefixes are the patterns RedactSecrets scans for. Order is
// irrelevant for the per-byte walk except that longer members of a family
// (`sk-ant-`, `sk-proj-`) must precede the bare fallback (`sk-`) so the
// longest match wins. Sourced from upstream issue tracker guidance (#1006)
// and established external secret-scanner conventions (GitHub Advanced
// Security, AWS Trusted Advisor).
//
// Covered providers: Anthropic (`sk-ant-`), OpenAI project + legacy
// (`sk-proj-` / `sk-`), GitHub PAT/OAuth (`ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`),
// GitHub fine-grained (`github_pat_`), GitLab (`glpat-`), AWS access keys
// (`AKIA`/`ASIA`), Slack (`xoxb-`/`xoxp-`/`xoxa-`/`xoxs-`), HuggingFace
// (`hf_`), npm (`npm_`), GCP / Google OAuth access tokens (`ya29.`),
// Databricks (`dapi`), HashiCorp Vault (`hvs.`), Stripe secret + restricted
// keys (`sk_live_`/`sk_test_`/`rk_live_`/`rk_test_`).
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
	// Databricks personal-access tokens (`dapi…`). Always 32 hex chars
	// following the 4-byte prefix.
	{prefix: "dapi", minTail: 16},
	// HashiCorp Cloud Platform (HCP) Vault service tokens (`hvs.…`). The
	// `.` is part of the prefix; the base64url body that follows may
	// contain `-` and `_` which isSecretTokenByte handles.
	{prefix: "hvs.", minTail: 16},
	// Stripe live and test secret keys (`sk_live_…` / `sk_test_…`).
	// Placed after the bare `sk-` family to avoid prefix-order confusion;
	// these use underscores and are unambiguously Stripe-shaped.
	{prefix: "sk_live_", minTail: 16},
	{prefix: "sk_test_", minTail: 16},
	// Stripe restricted keys (`rk_live_…` / `rk_test_…`). Restricted keys
	// carry a scoped subset of secret-key permissions but can still initiate
	// Stripe API calls, so leaking them is equally sensitive.
	{prefix: "rk_live_", minTail: 16},
	{prefix: "rk_test_", minTail: 16},
	// PEM / PKCS private-key and certificate headers (`-----BEGIN …-----`).
	// The tail scan stops at the first space (space is not an isSecretTokenByte
	// char), so minTail=0: the prefix itself is unambiguous enough — any
	// `-----BEGIN` in output signals PEM data. The redaction covers the
	// `-----BEGIN` token; the remainder of the header line and base64 body
	// lines are not redacted by this scanner (architecture limitation: the
	// tail scanner is token-word based). Deliberately NOT adding `eyJ` (JWT):
	// eyJ is base64(`{"`) and would false-positive on any base64-encoded JSON;
	// JWT dots are not isSecretTokenByte chars so the tail scan would terminate
	// immediately, making the match useless in practice.
	{prefix: "-----BEGIN", minTail: 0},
}

// secretPrefixesByFirstByte indexes secretPrefixes by their first byte so the
// RedactSecrets inner loop only probes the 1-3 candidates that can possibly
// match the current byte instead of all 24 prefixes (R20260609-PERF-9 #1976).
// Each bucket preserves secretPrefixes declaration order, so longest-match-wins
// (`sk-ant-` before `sk-`) still holds within a bucket. Built once at package
// init; never mutated afterwards so it is safe for concurrent reads.
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
// `…[truncated]` so dashboard / SIEM filters can spot redactions
// independently of length-truncation.
const secretRedactedMarker = "[REDACTED]"

// envAssignmentRe matches `KEY=value` (and `KEY = value`) assignments whose
// KEY name carries a sensitive marker (SECRET/TOKEN/PASSWORD/…), capturing the
// value run so RedactSecrets can mask it even when the value is NOT a
// well-known token prefix (R202606-SEC-008b, #2165). A cron traceback failing
// with `MY_CUSTOM_SECRET=hunter2` would otherwise transit verbatim to the
// authenticated dashboard.
//
// Scope is deliberately narrow to honour the package-level false-positive
// constraint: only keys NAMED like a credential match, so benign config such
// as `LOG_LEVEL=debug`, `PATH=/usr/bin`, or a generic `KEY=foo` is left
// untouched. The value run is `\S+` (stops at whitespace), so multi-token
// prose after the value is preserved. group 3 (the value) is replaced; the key
// and `=` are kept for operator diagnostics. Idempotent: `[REDACTED]` is `\S+`
// and re-masks to itself.
var envAssignmentRe = regexp.MustCompile(`(?i)\b([A-Z0-9_]*(SECRET|TOKEN|PASSWORD|PASSWD|CREDENTIAL|API_?KEY|ACCESS_?KEY|PRIVATE_?KEY|AUTH))\s*=\s*(\S+)`)

// RedactSecrets walks s once, swapping any occurrence of a well-known
// secret-prefix pattern for `[REDACTED]`. Returns the original (aliased)
// string when no prefix matched, so clean output pays only the single byte
// scan over the prefix table. Idempotent.
func RedactSecrets(s string) string {
	if s == "" {
		return s
	}
	// KEY=value env-assignment masking runs first so that a sensitive value
	// which is NOT a registered token prefix (e.g. `MY_SECRET=hunter2`) still
	// gets scrubbed. Gated on a cheap IndexByte('=') so the common no-`=` path
	// pays nothing (#2165).
	s = redactEnvAssignments(s)
	// Cheap early-bail: most output never contains a first byte of any
	// prefix; in that case the function aliases the input without allocating.
	if !mayContainSecretPrefix(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		matched := false
		// R20260609-PERF-9 (#1976): only the prefixes whose first byte equals
		// s[i] can match here, so probe that bucket (typically 1-3 entries)
		// instead of all 24 prefixes. A byte with no registered prefix (the
		// common case for an IndexAny first-byte hit landing on prose) skips
		// the inner loop entirely.
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

// isSecretTokenByte reports whether b is a legal continuation byte of a
// secret tail. Tokens we redact are alphanumerics + `-` + `_`; anything else
// (whitespace, punctuation, control) terminates the run.
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

// mayContainSecretPrefix is a fast pre-scan: returns false if no first byte
// of any registered prefix appears in s. Lets the common no-secret path skip
// the full prefix walk + string Builder allocation.
//
// First-byte set: 's' (sk-…/sk_live_/sk_test_), 'g' (ghp_/gho_/…/glpat-),
// 'A' (AKIA/ASIA), 'x' (xoxb-/…), 'h' (hf_/hvs.), 'n' (npm_), 'y' (ya29.),
// 'd' (dapi), 'r' (rk_live_/rk_test_), '-' (-----BEGIN). Keep in sync with
// secretPrefixes. strings.IndexAny uses a SIMD-backed byteset scan on
// amd64/arm64.
func mayContainSecretPrefix(s string) bool {
	return strings.IndexAny(s, "sgAxhnydr-") >= 0
}

// redactEnvAssignments masks the value of any `KEY=value` assignment whose KEY
// name looks like a credential (see envAssignmentRe). Returns the input
// aliased without allocation when no `=` is present (the common case), so the
// hot path stays zero-alloc. Idempotent. (R202606-SEC-008b, #2165).
func redactEnvAssignments(s string) string {
	if strings.IndexByte(s, '=') < 0 {
		return s
	}
	return envAssignmentRe.ReplaceAllStringFunc(s, func(m string) string {
		idx := strings.IndexByte(m, '=')
		if idx < 0 {
			return m
		}
		// Preserve `KEY` + any spaces + `=` + any spaces; mask only the value
		// run so operator diagnostics keep the (non-sensitive) key name.
		valStart := idx + 1
		for valStart < len(m) && (m[valStart] == ' ' || m[valStart] == '\t') {
			valStart++
		}
		// A single leading quote delimits the value but is not part of the
		// secret; keep it so quoted env dumps (and the PEM `-----BEGIN`
		// scanner downstream) render cleanly.
		if valStart < len(m) && (m[valStart] == '"' || m[valStart] == '\'') {
			valStart++
		}
		return m[:valStart] + secretRedactedMarker
	})
}
