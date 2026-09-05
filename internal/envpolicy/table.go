package envpolicy

// The single environment-variable policy table. Every env allow/deny decision
// naozhi makes — which keys the shim forwards to CLI subprocesses, which keys
// an access-profile overlay may set, which keys ~/.claude/settings.json may
// inject into the naozhi process, and which names ${VAR} expansion in
// config.yaml will resolve — is answered by Allowed against this one table.
//
// A Rule matches an exact key ("HOME"), a namespace ("AWS_*"), or a name
// suffix ("*_TOKEN"). Sources it does not mention fall through to the next
// less-specific matching rule (exact > longest prefix > suffix) and finally
// to the per-source default: SourceExpansion defaults to allow (any
// project-local variable may be interpolated unless it looks like a secret),
// every other source defaults to deny.
//
// The same key may legitimately get different answers per source — they are
// different trust domains. AWS_PROFILE is the canonical case: the shim
// forwards it (value-validated) because Bedrock auth needs it, but
// settings.json (writable by a Claude tool) and access profiles must not
// redirect naozhi's own credential source, and ${AWS_PROFILE} must never be
// materialised into config.yaml.

import "strings"

// Source identifies which pipeline is asking whether an env key is allowed.
type Source uint8

const (
	// SourceShim — env forwarded by the shim to CLI subprocesses
	// (FilterShimEnv / MergeShimEnv).
	SourceShim Source = 1 << iota
	// SourceOverlay — access-profile env overlays from project config
	// (ValidateOverlayEntry).
	SourceOverlay
	// SourceSettings — the ~/.claude/settings.json env section injected into
	// the naozhi parent process (cmd/naozhi filterClaudeEnv).
	SourceSettings
	// SourceExpansion — ${VAR} interpolation in config.yaml
	// (internal/config expandEnvVars). Matching is case-insensitive.
	SourceExpansion
)

// guardKind labels the value-validation family a per-source guard belongs to,
// so callers (e.g. the shim's drop-loggers) can name the guard in logs
// without a second key→guard table.
type guardKind uint8

const (
	guardNone guardKind = iota
	// guardProfile — AWS profile-name charset (credential_process injection).
	guardProfile
	// guardCredPath — absolute, traversal-free, NUL-free credential file path.
	guardCredPath
	// guardShimEndpoint — strict endpoint-URL SSRF guard (no private-IP
	// escape hatch, 0.0.0.0/:: rejected).
	guardShimEndpoint
	// guardBaseURL — ValidateBaseURLValue (honours NAOZHI_ALLOW_PRIVATE_BASE_URL).
	guardBaseURL
	// guardRegion — AWS region token charset (empty allowed).
	guardRegion
)

// guard couples a value check with its family label. check is nil for
// guardNone.
type guard struct {
	kind  guardKind
	check func(value string) error
}

// Rule is one row of the policy table.
//
// Pattern is an exact key, "PREFIX_*" (name prefix) or "*_SUFFIX" (name
// suffix). Specified is the set of sources this rule decides; Allowed is the
// subset of those that may set/expand the key (Allowed ⊆ Specified — a
// matching rule with the bit in Specified but not in Allowed is an explicit
// deny). Guards holds optional per-source value checks applied after the
// key-level allow.
type Rule struct {
	Pattern   string
	Specified Source
	Allowed   Source
	Guards    map[Source]guard
}

// Guard bundles for reuse across rules.
var (
	profileGuard      = guard{guardProfile, func(v string) error { return errIfUnsafeProfile(v) }}
	credPathGuard     = guard{guardCredPath, func(v string) error { return errIfUnsafeCredPath(v) }}
	shimEndpointGuard = guard{guardShimEndpoint, validateShimEndpointURL}
	baseURLGuard      = guard{guardBaseURL, ValidateBaseURLValue}
	regionGuard       = guard{guardRegion, func(v string) error { return errIfUnsafeRegion(v) }}
)

// Table is the policy. Ordering inside the slice does not matter; resolution
// is by specificity (exact > longest prefix > suffix), see Allowed.
//
// Provenance of each verdict (equivalence-migrated 2026-09-05, #2531): the
// shim column reproduces the former shim allowlist, the overlay column the
// former access-profile allowlist, the settings column the former
// settings.json prefix-allow/deny lists, and the expansion column the former
// config.yaml deny/allow prefix + deny-suffix lists. The table-driven tests
// in table_test.go pin every cell against copies of the retired lists.
var Table = []Rule{
	// ── system essentials (shim-forwarded plumbing) ────────────────────────
	{Pattern: "HOME", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "USER", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "LOGNAME", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "PATH", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "SHELL", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "TERM", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "TMPDIR", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "TMP", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "TEMP", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "LANG", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "LC_*", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "TZ", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "XDG_*", Specified: SourceShim, Allowed: SourceShim},

	// ── Claude CLI / Anthropic ─────────────────────────────────────────────
	// Explicit keys for shim/overlay, never the whole namespace: a future or
	// namespace-sharing variable would otherwise be readable via the Bash
	// tool. settings.json gets the namespaces (its values only enter the
	// naozhi parent process, not CLI children).
	{Pattern: "ANTHROPIC_*", Specified: SourceSettings | SourceExpansion, Allowed: SourceSettings},
	{Pattern: "CLAUDE_*", Specified: SourceSettings | SourceExpansion, Allowed: SourceSettings},
	{Pattern: "ANTHROPIC_API_KEY", Specified: SourceShim | SourceOverlay, Allowed: SourceShim | SourceOverlay},
	{Pattern: "ANTHROPIC_AUTH_TOKEN", Specified: SourceShim | SourceOverlay, Allowed: SourceShim | SourceOverlay},
	{Pattern: "CLAUDE_CODE_OAUTH_TOKEN", Specified: SourceShim | SourceOverlay, Allowed: SourceShim | SourceOverlay},
	{Pattern: "ANTHROPIC_MODEL", Specified: SourceShim | SourceOverlay, Allowed: SourceShim | SourceOverlay},
	{
		Pattern:   "ANTHROPIC_BASE_URL",
		Specified: SourceShim | SourceOverlay | SourceSettings,
		Allowed:   SourceShim | SourceOverlay | SourceSettings,
		Guards: map[Source]guard{
			SourceShim:     shimEndpointGuard,
			SourceOverlay:  baseURLGuard,
			SourceSettings: baseURLGuard,
		},
	},
	{
		Pattern:   "ANTHROPIC_BEDROCK_BASE_URL",
		Specified: SourceShim | SourceOverlay | SourceSettings,
		Allowed:   SourceShim | SourceOverlay | SourceSettings,
		Guards: map[Source]guard{
			SourceShim:     shimEndpointGuard,
			SourceOverlay:  baseURLGuard,
			SourceSettings: baseURLGuard,
		},
	},
	{
		Pattern:   "ANTHROPIC_VERTEX_BASE_URL",
		Specified: SourceSettings,
		Allowed:   SourceSettings,
		Guards:    map[Source]guard{SourceSettings: baseURLGuard},
	},
	{Pattern: "CLAUDE_CODE_USE_BEDROCK", Specified: SourceShim | SourceOverlay, Allowed: SourceShim | SourceOverlay},
	{Pattern: "CLAUDE_CODE_SKIP_BEDROCK_AUTH", Specified: SourceShim | SourceOverlay, Allowed: SourceShim | SourceOverlay},
	{Pattern: "CLAUDE_BIN", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "CLAUDE_MODEL", Specified: SourceShim, Allowed: SourceShim},
	// CLI kill-switch / mock-mode keys inside the settings-allowed CLAUDE_
	// namespace: settings.json is writable by a Claude tool and these change
	// security-relevant behaviour of shim/CLI children (#1660).
	{Pattern: "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", Specified: SourceSettings, Allowed: 0},
	{Pattern: "CLAUDE_CODE_USE_MOCK_RESPONSES", Specified: SourceSettings, Allowed: 0},
	// *_FILE indirection keys (committable host path in place of a secret):
	// overlay-only concept, value must be a safe absolute path.
	{
		Pattern:   "ANTHROPIC_AUTH_TOKEN_FILE",
		Specified: SourceOverlay,
		Allowed:   SourceOverlay,
		Guards:    map[Source]guard{SourceOverlay: credPathGuard},
	},
	{
		Pattern:   "ANTHROPIC_API_KEY_FILE",
		Specified: SourceOverlay,
		Allowed:   SourceOverlay,
		Guards:    map[Source]guard{SourceOverlay: credPathGuard},
	},
	{
		Pattern:   "CLAUDE_CODE_OAUTH_TOKEN_FILE",
		Specified: SourceOverlay,
		Allowed:   SourceOverlay,
		Guards:    map[Source]guard{SourceOverlay: credPathGuard},
	},

	// ── AWS (Bedrock auth) ─────────────────────────────────────────────────
	// settings.json gets the namespace minus the keys that would change
	// naozhi's own AWS auth source (role switching, credential-file
	// redirection): settings.json can be written by a Claude tool, so
	// allowing those equals a credential-hijack channel.
	{Pattern: "AWS_*", Specified: SourceSettings | SourceExpansion, Allowed: SourceSettings},
	{
		Pattern:   "AWS_REGION",
		Specified: SourceShim | SourceOverlay,
		Allowed:   SourceShim | SourceOverlay,
		Guards:    map[Source]guard{SourceOverlay: regionGuard},
	},
	{
		Pattern:   "AWS_DEFAULT_REGION",
		Specified: SourceShim | SourceOverlay,
		Allowed:   SourceShim | SourceOverlay,
		Guards:    map[Source]guard{SourceOverlay: regionGuard},
	},
	{Pattern: "AWS_ACCESS_KEY_ID", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "AWS_SECRET_ACCESS_KEY", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "AWS_SESSION_TOKEN", Specified: SourceShim, Allowed: SourceShim},
	// AWS_PROFILE: the one key with three different answers by design. The
	// shim forwards it (profile-name charset guard) because Bedrock auth may
	// need a named profile; settings.json and overlays must not repoint
	// naozhi's credential source (2026-07-19 incident).
	{
		Pattern:   "AWS_PROFILE",
		Specified: SourceShim | SourceSettings,
		Allowed:   SourceShim,
		Guards:    map[Source]guard{SourceShim: profileGuard},
	},
	// AWS_DEFAULT_PROFILE is not forwarded by the shim (it never made the
	// allowlist); the profile guard is still attached so the defensive
	// validator keeps covering it.
	{
		Pattern:   "AWS_DEFAULT_PROFILE",
		Specified: SourceShim | SourceSettings,
		Allowed:   0,
		Guards:    map[Source]guard{SourceShim: profileGuard},
	},
	{
		Pattern:   "AWS_SHARED_CREDENTIALS_FILE",
		Specified: SourceShim | SourceSettings,
		Allowed:   SourceShim,
		Guards:    map[Source]guard{SourceShim: credPathGuard},
	},
	{
		Pattern:   "AWS_CONFIG_FILE",
		Specified: SourceShim | SourceSettings,
		Allowed:   SourceShim,
		Guards:    map[Source]guard{SourceShim: credPathGuard},
	},
	{Pattern: "AWS_ROLE_ARN", Specified: SourceShim | SourceSettings, Allowed: SourceShim},
	{
		Pattern:   "AWS_WEB_IDENTITY_TOKEN_FILE",
		Specified: SourceShim | SourceSettings,
		Allowed:   SourceShim,
		Guards:    map[Source]guard{SourceShim: credPathGuard},
	},
	{
		Pattern:   "AWS_ENDPOINT_URL",
		Specified: SourceShim | SourceSettings,
		Allowed:   SourceShim,
		Guards:    map[Source]guard{SourceShim: shimEndpointGuard},
	},
	// AWS_BEDROCK_ENDPOINT stays settings-allowed via the AWS_* namespace
	// (it is not in the settings deny set), so only the shim side is pinned
	// here.
	{
		Pattern:   "AWS_BEDROCK_ENDPOINT",
		Specified: SourceShim,
		Allowed:   SourceShim,
		Guards:    map[Source]guard{SourceShim: shimEndpointGuard},
	},
	{Pattern: "AWS_CA_BUNDLE", Specified: SourceSettings, Allowed: 0},

	// ── proxies (settings.json → naozhi parent process) ────────────────────
	// Proxy values steer ALL outbound traffic, so they get the same
	// non-loopback-https guard as base URLs (#1660). NO_PROXY is not a URL.
	{Pattern: "HTTP_PROXY", Specified: SourceSettings, Allowed: SourceSettings, Guards: map[Source]guard{SourceSettings: baseURLGuard}},
	{Pattern: "HTTPS_PROXY", Specified: SourceSettings, Allowed: SourceSettings, Guards: map[Source]guard{SourceSettings: baseURLGuard}},
	{Pattern: "http_proxy", Specified: SourceSettings, Allowed: SourceSettings, Guards: map[Source]guard{SourceSettings: baseURLGuard}},
	{Pattern: "https_proxy", Specified: SourceSettings, Allowed: SourceSettings, Guards: map[Source]guard{SourceSettings: baseURLGuard}},
	{Pattern: "NO_PROXY", Specified: SourceSettings, Allowed: SourceSettings},
	{Pattern: "no_proxy", Specified: SourceSettings, Allowed: SourceSettings},

	// ── git (shim-forwarded; explicit keys, never GIT_*) ───────────────────
	{Pattern: "SSH_AUTH_SOCK", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "GIT_AUTHOR_NAME", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "GIT_AUTHOR_EMAIL", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "GIT_COMMITTER_NAME", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "GIT_COMMITTER_EMAIL", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "GIT_CONFIG_GLOBAL", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "GIT_CONFIG_SYSTEM", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "GIT_DIR", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "GIT_WORK_TREE", Specified: SourceShim, Allowed: SourceShim},

	// ── dev toolchains (shim-forwarded; explicit keys) ─────────────────────
	{Pattern: "GOPATH", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "GOROOT", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "GOBIN", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "CARGO_HOME", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "RUSTUP_HOME", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "NODE_ENV", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "NPM_CONFIG_REGISTRY", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "NPM_TOKEN", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "PYTHONDONTWRITEBYTECODE", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "PYTHONUNBUFFERED", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "CONDA_PREFIX", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "CONDA_DEFAULT_ENV", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "CONDA_SHLVL", Specified: SourceShim, Allowed: SourceShim},
	{Pattern: "JAVA_HOME", Specified: SourceShim, Allowed: SourceShim},

	// ── ${VAR} expansion in config.yaml ────────────────────────────────────
	// Upstream-credential namespaces are never expanded (a materialised key
	// could be logged or echoed by the dashboard, #1047). ANTHROPIC_* /
	// CLAUDE_* / AWS_* already carry the expansion deny above.
	{Pattern: "AZURE_*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "GCP_*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "GOOGLE_*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "OCI_*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "OPENAI_*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "GITHUB_TOKEN*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "GH_TOKEN*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "OPENROUTER_*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "MISTRAL_*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "HUGGINGFACE_*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "HUGGING_FACE_*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "SECRET_*", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "PASSWORD_*", Specified: SourceExpansion, Allowed: 0},
	// naozhi-owned namespaces whose values ARE the legitimate config inputs;
	// as prefix rules they outrank the deny suffixes below (#2320) but a
	// deny-prefix above always wins because no namespace overlaps one.
	{Pattern: "NAOZHI_*", Specified: SourceExpansion, Allowed: SourceExpansion},
	{Pattern: "FEISHU_*", Specified: SourceExpansion, Allowed: SourceExpansion},
	{Pattern: "SLACK_*", Specified: SourceExpansion, Allowed: SourceExpansion},
	{Pattern: "PC_*", Specified: SourceExpansion, Allowed: SourceExpansion},
	{Pattern: "IM_*", Specified: SourceExpansion, Allowed: SourceExpansion},
	// Generic secret-naming suffixes (DATABASE_PASSWORD, SOME_API_TOKEN)
	// refused regardless of prefix; kept conservative so non-secret aliases
	// stay usable.
	{Pattern: "*_SECRET", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "*_PASSWORD", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "*_PASSWD", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "*_TOKEN", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "*_APIKEY", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "*_API_KEY", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "*_ACCESS_KEY", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "*_SECRET_KEY", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "*_PRIVATE_KEY", Specified: SourceExpansion, Allowed: 0},
	{Pattern: "*_CREDENTIALS", Specified: SourceExpansion, Allowed: 0},
}

// Allowed reports whether from may set (or expand) key, returning the rule
// that decided. When no matching rule specifies the source, the zero Rule is
// returned with the per-source default: allow for SourceExpansion, deny
// otherwise. Value guards are NOT applied here — callers run
// GuardFor(key, from) on the value after the key-level allow.
func Allowed(key string, from Source) (Rule, bool) {
	if from == SourceExpansion {
		// The expansion pipeline has always matched case-insensitively
		// (an operator may write ${aws_profile}).
		key = strings.ToUpper(key)
	}
	if r, ok := lookup(key, from); ok {
		return r, r.Allowed&from != 0
	}
	return Rule{}, from == SourceExpansion
}

// GuardFor returns the value check attached to key for from, or nil.
func GuardFor(key string, from Source) func(string) error {
	if r, ok := lookup(key, from); ok {
		if g, ok := r.Guards[from]; ok {
			return g.check
		}
	}
	return nil
}

// guardKindFor returns the guard family attached to key for from (guardNone
// when absent). Used by the shim drop-loggers to name the failing guard.
func guardKindFor(key string, from Source) guardKind {
	if r, ok := lookup(key, from); ok {
		if g, ok := r.Guards[from]; ok {
			return g.kind
		}
	}
	return guardNone
}

// lookup finds the most specific rule that specifies from: an exact match
// first, then the longest matching "PREFIX_*", then the longest matching
// "*_SUFFIX".
func lookup(key string, from Source) (Rule, bool) {
	var (
		best      Rule
		bestOK    bool
		bestClass int // 3 exact, 2 prefix, 1 suffix
		bestLen   int
	)
	for _, r := range Table {
		if r.Specified&from == 0 {
			continue
		}
		class, n := 0, 0
		switch {
		case strings.HasSuffix(r.Pattern, "*"):
			p := strings.TrimSuffix(r.Pattern, "*")
			if strings.HasPrefix(key, p) {
				class, n = 2, len(p)
			}
		case strings.HasPrefix(r.Pattern, "*"):
			s := strings.TrimPrefix(r.Pattern, "*")
			if strings.HasSuffix(key, s) {
				class, n = 1, len(s)
			}
		default:
			if key == r.Pattern {
				class, n = 3, len(r.Pattern)
			}
		}
		if class == 0 {
			continue
		}
		if class > bestClass || (class == bestClass && n > bestLen) {
			best, bestOK, bestClass, bestLen = r, true, class, n
		}
	}
	return best, bestOK
}
