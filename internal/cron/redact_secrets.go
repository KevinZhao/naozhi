// Package cron — redact_secrets.go: well-known token-prefix scrubbing for
// CronRun.Result and Job.LastResult before persistence / WS broadcast.
//
// R234-SEC-7 (#1006). Job.LastResult is persisted to cron_jobs.json after
// truncateWithSuffix + SanitizeForLog, but no secret-pattern filter ran
// before this change. Claude / shell output may legitimately echo plaintext
// API tokens (`sk-ant-…`, `ghp_…`, `AKIA…`) — without scrubbing they leak
// to disk + dashboard WS broadcast. This file adds a single-pass byte scan
// that swaps known prefixes' suffix bytes for `[REDACTED]` while leaving
// the surrounding text intact so error classification and operator
// debugging stay readable.
//
// Design notes:
//
//   - Token-wise scan, NOT regex. `redactPathsInCronError` (sibling
//     redactor in scheduler_finish.go) made the same choice because every
//     cron tick + every TriggerNow flows through this hot path — a regex
//     compile per call (or even sync.Once'd globals) costs more than a
//     direct byte scan over the small number of well-known prefixes.
//   - Prefix list is conservative: only patterns whose first 4-7 bytes
//     are unambiguous markers of a secret (`sk-ant-`, `ghp_`, `AKIA`).
//     Generic password-like patterns are out of scope — false positives on
//     legitimate Claude output (a 20-char hash that happens to look
//     vendor-like) would corrupt operator diagnostics more than the rare
//     leak avoids.
//   - Idempotent: running the redactor twice produces the same string, so
//     finishRun's persistence pipeline (sanitiseRunResult → file write →
//     subsequent re-read → re-sanitise) cannot drift.
//   - Empty / no-prefix inputs return the aliased input without any
//     allocation (mirrors redactPathsInCronError's fast path).

package cron

import (
	"strings"
)

// secretPrefix names the well-known token prefixes redactSecretsInResult
// recognises. The runeAfter / lenAfter fields tune how many post-prefix
// bytes the redactor consumes before swapping in [REDACTED]: short prefixes
// (`AKIA`) need a fixed token length (16 alphanumerics); long prefixes
// (`sk-ant-`) carry a hyphen-delimited tail of variable length so the
// redactor consumes runes until the first non-token byte.
//
// minTail is a sanity floor: if the post-prefix byte run is shorter than
// minTail the prefix is treated as a literal substring rather than a
// secret (avoids redacting "ghp_" appearing in prose / a doc URL).
type secretPrefix struct {
	prefix  string
	minTail int
}

// secretPrefixes are the patterns redactSecretsInResult scans for. Order
// is irrelevant — the scan walks once per byte and matches the longest
// prefix whose start position lines up. Sourced from upstream issue tracker
// guidance (#1006) and the established external secret-scanner conventions
// (GitHub Advanced Security, AWS Trusted Advisor); operators with
// additional in-house token schemes can extend the list at the same point.
//
// Covered providers: Anthropic (`sk-ant-`), OpenAI project + legacy
// (`sk-proj-` / `sk-`), GitHub PAT/OAuth (`ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`),
// GitLab (`glpat-`), AWS access keys (`AKIA`/`ASIA`), Slack
// (`xoxb-`/`xoxp-`/`xoxa-`/`xoxs-`), HuggingFace (`hf_`), npm (`npm_`),
// GCP / Google OAuth access tokens (`ya29.`).
var secretPrefixes = []secretPrefix{
	// Anthropic API keys (`sk-ant-…`). The post-prefix tail is variable
	// length and may include hyphens, so minTail is generous.
	{prefix: "sk-ant-", minTail: 8},
	// OpenAI project keys (`sk-proj-…`). Placed before the bare `sk-`
	// fallback so the longer prefix matches first.
	{prefix: "sk-proj-", minTail: 16},
	// OpenAI legacy keys (`sk-…`, ~48 chars). Large minTail avoids
	// redacting the bare "sk-" substring in ordinary prose. Kept last
	// among the sk- family so sk-ant-/sk-proj- win the longest match.
	{prefix: "sk-", minTail: 40},
	// npm automation / publish tokens (`npm_…`).
	{prefix: "npm_", minTail: 16},
	// GitHub PATs / fine-grained tokens / OAuth.
	{prefix: "ghp_", minTail: 16},
	{prefix: "gho_", minTail: 16},
	{prefix: "ghu_", minTail: 16},
	{prefix: "ghs_", minTail: 16},
	{prefix: "ghr_", minTail: 16},
	// GitHub fine-grained personal access tokens (`github_pat_…`).
	{prefix: "github_pat_", minTail: 16},
	// GitLab personal-access tokens.
	{prefix: "glpat-", minTail: 16},
	// AWS access key IDs (`AKIA…` / `ASIA…` for STS). Always 16 base32
	// alphanumerics following the 4-byte prefix.
	{prefix: "AKIA", minTail: 16},
	{prefix: "ASIA", minTail: 16},
	// Slack tokens cover bot / user / app variants.
	{prefix: "xoxb-", minTail: 16},
	{prefix: "xoxp-", minTail: 16},
	{prefix: "xoxa-", minTail: 16},
	{prefix: "xoxs-", minTail: 16},
	// HuggingFace tokens (`hf_…`).
	{prefix: "hf_", minTail: 16},
	// GCP / Google OAuth access tokens (`ya29.…`). The `.` is part of the
	// prefix; the base64url body that follows (alphanumerics + `-`/`_`) is
	// consumed by isSecretTokenByte as the tail.
	{prefix: "ya29.", minTail: 16},
}

// secretRedactedMarker replaces matched secret bytes. Distinct from
// `…[truncated]` so dashboard / SIEM filters can spot redactions
// independently of length-truncation.
const secretRedactedMarker = "[REDACTED]"

// redactSecretsInResult walks s once, swapping any occurrence of a
// well-known secret-prefix pattern for `[REDACTED]`. Returns the original
// (aliased) string when no prefix matched, so a clean Claude output pays
// only the single byte scan over the prefix table.
//
// Idempotent: a second pass over an already-redacted string finds no
// prefix because `[REDACTED]` itself does not start with any registered
// marker.
func redactSecretsInResult(s string) string {
	if s == "" {
		return s
	}
	// Cheap early-bail: scan once for any first byte of a prefix. Most
	// cron output never contains an `s` / `g` / `A` / `x` / `h` / `n`
	// followed by the remaining prefix bytes; in that case the function
	// aliases the input without allocating.
	if !mayContainSecretPrefix(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		matched := false
		for _, sp := range secretPrefixes {
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
// secret tail. Tokens we redact are alphanumerics + `-` + `_`; anything
// else (whitespace, punctuation, control) terminates the run.
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
// of any registered prefix appears in s. Lets the common no-secret path
// skip the full prefix walk + string Builder allocation.
func mayContainSecretPrefix(s string) bool {
	// First-byte set: 's' (sk-…), 'g' (ghp_/gho_/…/glpat-), 'A' (AKIA/ASIA),
	// 'x' (xoxb-/…), 'h' (hf_), 'n' (npm_), 'y' (ya29.). Keep in sync with
	// secretPrefixes.
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case 's', 'g', 'A', 'x', 'h', 'n', 'y':
			return true
		}
	}
	return false
}
