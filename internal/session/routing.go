// Package session — routing.go
//
// KeyResolver centralises the (session key, AgentOpts) derivation used
// across dispatch / server / upstream when an inbound message, resume
// request, or planner-restart RPC needs to target a session.
//
// Design goal: make the aliasing-safe merge of AgentOpts.ExtraArgs an
// internal invariant (§2.2 of docs/rfc/key-resolver.md) rather than a
// caller responsibility. Seven call sites had diverged copies of this
// logic with subtly different guarantees — some missing `[:len:len]`
// aliasing protection, some missing planner model/prompt inheritance;
// the Resolver is the single source of truth.
//
// Dependency direction: PlannerDataSource is declared here so session
// never imports project. The project package ships a small adapter
// (internal/project.NewDataSource) that satisfies PlannerDataSource.
package session

import (
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
)

// maxPlannerPromptBytesAtSpawn caps the planner prompt that flows into
// `--append-system-prompt` argv at spawn time. Mirrors
// internal/project.MaxPlannerPromptBytes (8 KB) but kept in session so
// the dependency direction stays "project → session" only (session
// cannot import project per the routing.go doc contract).
//
// 8 KB is far below Linux ARG_MAX (~2 MB) but well above any realistic
// human-authored planner prompt; it bounds the worst case where a
// tampered project.yaml / config.yaml / CLAUDE.md slipped past the
// write-path validator (HTTP PUT, reverse-RPC update_config, Manager.Scan
// load-time ValidateConfig). R215-SEC-P1-2.
const maxPlannerPromptBytesAtSpawn = 8 * 1024

// SanitisePlannerPromptForSpawn is defense-in-depth: re-validate the
// PlannerPrompt at the spawn-routing site before it crosses the trust
// boundary into CLI argv (`--append-system-prompt <prompt>`). Returns ""
// for any rejected input so the spawn falls through to "no planner
// system prompt" rather than running with a poisoned one.
//
// Mirror of project.EffectivePlannerPrompt's rune-level guards PLUS the
// length cap that EffectivePlannerPrompt previously skipped (the
// write-path ValidateConfig caps length, but a tampered disk file or
// an in-memory mutation past the write-path could land an oversized
// value here). project = source of truth; session re-runs the same
// policy at the spawn boundary so any future code path that bypasses
// the project layer (test helpers, future RPC, etc.) still cannot
// inject control bytes or oversize argv.
//
// Called from both ResolveForChat (chat-view planner) and
// ResolveForPlannerKey (administrative planner restart) in this
// package. Exported (R215-SEC-P1-2 #535) so the server-package
// handlePlannerRestart legacy fallback (resolver==nil test path) can
// reuse the same sanitiser without rebuilding the policy in a second
// place — the alternative was for that fallback to feed
// EffectivePlannerPrompt straight into AgentOpts.ExtraArgs, which
// bypasses the spawn-boundary check the resolver path enforces.
func SanitisePlannerPromptForSpawn(prompt, projectName string) string {
	return sanitisePlannerPromptForSpawn(prompt, projectName)
}

// sanitisePlannerPromptForSpawn is the unexported in-package implementation;
// SanitisePlannerPromptForSpawn re-exports it for cross-package callers.
func sanitisePlannerPromptForSpawn(prompt, projectName string) string {
	if prompt == "" {
		return ""
	}
	if len(prompt) > maxPlannerPromptBytesAtSpawn {
		slog.Warn("planner prompt exceeds spawn-time length cap; dropping",
			"project", projectName,
			"len", len(prompt),
			"cap", maxPlannerPromptBytesAtSpawn)
		return ""
	}
	if !utf8.ValidString(prompt) {
		slog.Warn("planner prompt contains invalid UTF-8 at spawn; dropping",
			"project", projectName)
		return ""
	}
	for i := 0; i < len(prompt); i++ {
		c := prompt[i]
		// 0x09 tab / 0x0A LF / 0x0D CR are legitimate markdown content;
		// other C0 + NUL + DEL would truncate argv on execve or corrupt
		// stream-json framing at the shim boundary.
		if c == 0 || (c < 0x20 && c != 0x09 && c != 0x0a && c != 0x0d) || c == 0x7f {
			slog.Warn("planner prompt contains control byte at spawn; dropping",
				"project", projectName, "byte", c)
			return ""
		}
	}
	for _, r := range prompt {
		if osutil.IsLogInjectionRune(r) {
			slog.Warn("planner prompt contains injection rune (C1/bidi/LS-PS) at spawn; dropping",
				"project", projectName)
			return ""
		}
	}
	return prompt
}

// PlannerDataSource abstracts the project-layer data KeyResolver needs.
// Concrete implementation lives in the project package; session never
// imports project directly. All methods return fully-snapshot'd values
// so callers can treat them as pure reads (no hidden mutex state bleed).
type PlannerDataSource interface {
	// ProjectBinding returns the project metadata for the given IM chat,
	// or zero-value (Bound == false) if the chat is not bound.
	ProjectBinding(platform, chatType, chatID string) ProjectBinding

	// ProjectByName returns the project metadata for the given planner
	// key's embedded project name. Used by the key-reverse path.
	// ok == false when the project cannot be found (e.g. operator
	// deleted it between RPC arrival and restart).
	ProjectByName(name string) (ProjectBinding, bool)
}

// ProjectBinding is the minimal projection session needs. Populated by
// the project-package adapter via EffectivePlannerModel /
// EffectivePlannerPrompt, so the Resolver does NOT re-implement those
// precedence rules (they stay authoritative in project.Manager).
type ProjectBinding struct {
	Bound         bool
	Name          string
	WorkspaceDir  string
	PlannerModel  string // "" = inherit router / AgentDefaults
	PlannerPrompt string // "" = no --append-system-prompt
}

// KeyResolver derives a (session key, AgentOpts) pair for a given
// dispatch context. It encodes the project-binding precedence
// (general → planner, non-general → workspace-only) and the ExtraArgs
// aliasing-safe merge as internal invariants.
//
// The zero value is not usable; construct via NewKeyResolver.
type KeyResolver struct {
	defaults map[string]AgentOpts // agentID -> base opts
	data     PlannerDataSource    // nil → project feature disabled
}

// NewKeyResolver constructs a resolver. data may be nil to disable
// project-aware routing; in that case all Resolve* methods behave as
// if no chat is ever project-bound.
func NewKeyResolver(defaults map[string]AgentOpts, data PlannerDataSource) *KeyResolver {
	return &KeyResolver{defaults: defaults, data: data}
}

// ResolveForChat is the "chat-view" path: given IM chat coordinates and
// agentID, return the routed key and merged opts. Replaces #1 (dispatch
// main) and #3 (/urgent) in docs/rfc/key-resolver.md §4.
//
// Precedence (see §3.3 table):
//   - unbound chat → base = defaults[agentID], standard IM key
//   - bound chat + agentID != "general" → base, overlay Workspace only,
//     Exempt explicitly set to false (defense-in-depth).
//   - bound chat + agentID == "general" → base, overlay Workspace /
//     Model / Prompt, Exempt = true, planner key.
//
// ExtraArgs merge uses three-arg slice (`[:len:len]`) to force a fresh
// backing array — without this, two concurrent goroutines reading the
// same `defaults[agentID].ExtraArgs` would corrupt each other's opts
// when cap > len (R37-CONCUR1).
//
// Aliasing safety extends to ALL return paths, not just the planner
// branch that appends — even the "early return" paths clone ExtraArgs
// so a downstream caller that does `opts.ExtraArgs = append(...)` on
// the returned slice cannot poison the shared defaults map. Without
// this clone, R215-ARCH-P2-8: a non-planner caller appending to opts
// would silently mutate r.defaults[agentID].ExtraArgs whenever
// cap > len.
func (r *KeyResolver) ResolveForChat(platform, chatType, chatID, agentID string) (key string, opts AgentOpts) {
	base := r.defaults[agentID] // zero-value safe
	// Defensive clone of the shared backing array. cheap (typical
	// ExtraArgs is empty or 1-2 entries) and removes a subtle aliasing
	// foot-gun for callers further down the chain.
	base.ExtraArgs = slices.Clone(base.ExtraArgs)

	if r.data == nil {
		return SessionKey(platform, chatType, chatID, agentID), base
	}

	b := r.data.ProjectBinding(platform, chatType, chatID)
	if !b.Bound {
		return SessionKey(platform, chatType, chatID, agentID), base
	}

	if agentID != "general" {
		// Non-general agent: reuse workspace only; do NOT inherit
		// planner model/prompt. Exempt explicitly false so stale
		// defaults configuration cannot accidentally promote this
		// session to exempt.
		base.Workspace = b.WorkspaceDir
		base.Exempt = false
		return SessionKey(platform, chatType, chatID, agentID), base
	}

	// general agent + bound project ⇒ planner (chat-view).
	base.Exempt = true
	base.Workspace = b.WorkspaceDir
	if b.PlannerModel != "" {
		base.Model = b.PlannerModel
	}
	// R215-SEC-P1-2: re-validate at spawn boundary. Drops oversized /
	// control-char prompts that could have slipped past the write-path
	// ValidateConfig (tampered disk file, future bypass path).
	if pp := sanitisePlannerPromptForSpawn(b.PlannerPrompt, b.Name); pp != "" {
		// Three-arg slice forces fresh backing array. Without
		// `:len:len`, append would write past len in the shared
		// defaults slice when cap > len — see
		// dispatch/planner_args_isolation_test.go for canary test.
		base.ExtraArgs = append(
			base.ExtraArgs[:len(base.ExtraArgs):len(base.ExtraArgs)],
			"--append-system-prompt", pp,
		)
	}
	return plannerKeyFor(b.Name), base
}

// ResolveForPlannerKey is the "planner-view" path: from a project name,
// return the planner key and opts WITHOUT reading defaults[agentID].
// Used by administrative restart flows (#6 HTTP planner-restart, #7
// reverse-RPC restart_planner).
//
// Deliberately does NOT inherit from defaults["general"]: planner
// restart's spec is to start from a blank opts and layer only project
// configuration on top. If a future change wants to change this to
// chat-view semantics, the contract is visible here — not buried in
// each call site. See docs/rfc/key-resolver.md §2.2 for the rationale.
//
// Returns ok=false when the project cannot be found. Callers should
// surface 404 (HTTP) or error (RPC); they must NOT fall back to
// chat-view behaviour.
func (r *KeyResolver) ResolveForPlannerKey(projectName string) (key string, opts AgentOpts, ok bool) {
	if r.data == nil {
		return "", AgentOpts{}, false
	}
	b, found := r.data.ProjectByName(projectName)
	if !found {
		return "", AgentOpts{}, false
	}
	opts = AgentOpts{
		Exempt:    true,
		Workspace: b.WorkspaceDir,
		Model:     b.PlannerModel,
	}
	// R215-SEC-P1-2: same defense-in-depth as ResolveForChat. The
	// planner-restart RPC paths (administrative HTTP / reverse-RPC) can
	// also land here with a stale b.PlannerPrompt cached from a prior
	// disk reload, so the boundary check must guard both entry points.
	if pp := sanitisePlannerPromptForSpawn(b.PlannerPrompt, b.Name); pp != "" {
		// Fresh literal slice; no aliasing risk because we do not
		// read from defaults.
		opts.ExtraArgs = []string{"--append-system-prompt", pp}
	}
	return plannerKeyFor(b.Name), opts, true
}

// ResolveForKey is the "key-resume" path: given an existing key from
// sessions.json or dashboard WS subscribe, return the AgentOpts used
// for re-spawning. Replaces #5 (buildSessionOpts) in
// docs/rfc/key-resolver.md §4.
//
// Dispatches four branches:
//   - planner key ("project:{name}:planner") → delegate to
//     ResolveForPlannerKey; ok reflects whether the project still
//     exists.
//   - other reserved namespaces (cron: / scratch:) → ok=false; caller
//     must route to the namespace's dedicated resolution path.
//   - IM 4-segment key → ok=true, opts = defaults[agentID]. Notably
//     does NOT overlay workspace (the sessions.json / WS subscribe
//     path carries workspace independently).
//   - malformed (non-4-segment, non-reserved) → ok=false.
//
// The "IM 4-segment" branch intentionally diverges from ResolveForChat:
// resume-from-key has no fresh chat context, so project binding lookup
// would produce stale workspace overrides. §4.5 of the RFC.
func (r *KeyResolver) ResolveForKey(key string) (opts AgentOpts, ok bool) {
	if isPlannerKey(key) {
		name := plannerNameFromKey(key)
		_, planOpts, plannerOK := r.ResolveForPlannerKey(name)
		return planOpts, plannerOK
	}
	if IsReservedNamespace(key) {
		// cron: / scratch: — resume has its own paths.
		return AgentOpts{}, false
	}
	parts := strings.SplitN(key, ":", 4)
	if len(parts) != 4 {
		return AgentOpts{}, false
	}
	return r.defaults[parts[3]], true
}

// KeyForChat is the lightweight key-only variant for callers that do
// not need opts (e.g. /stop, /new). Does not compute opts. Kept
// separate so repeat key-only calls (/new {agent1} /new {agent2} ...)
// do not pay the opts merge cost or the ProjectBinding lookup.
//
// For project-bound chats with agentID=="general", this returns the
// planner key. Otherwise returns the standard IM key.
func (r *KeyResolver) KeyForChat(platform, chatType, chatID, agentID string) string {
	if r.data != nil && agentID == "general" {
		b := r.data.ProjectBinding(platform, chatType, chatID)
		if b.Bound {
			return plannerKeyFor(b.Name)
		}
	}
	return SessionKey(platform, chatType, chatID, agentID)
}

// ProjectBindingForChat exposes the resolver's project lookup so dispatch
// slash-command UX paths (/cd, /pwd echo, /new echo) can read the bound
// project's name / workspace / planner config from the same source the
// session-key derivation uses, instead of reaching for *project.Manager
// independently. R218B-ARCH-2 (#648): pre-fix, dispatch held both
// resolver and projectMgr — a concurrent BindChat / UnbindAllChat could
// race the IM hot path's resolver-derived key against a stale
// ProjectForChat read in the same handler. Funnelling read paths through
// the resolver keeps the two sources reading the same snapshot from the
// PlannerDataSource adapter.
//
// Returns ProjectBinding{Bound: false} when the resolver has no data
// source wired (test/headless construction with NewKeyResolver(nil, nil))
// or when the chat is genuinely unbound. Callers should branch on
// b.Bound rather than nil-checking the resolver, mirroring the
// ProjectBinding zero-value contract documented on PlannerDataSource.
func (r *KeyResolver) ProjectBindingForChat(platform, chatType, chatID string) ProjectBinding {
	if r.data == nil {
		return ProjectBinding{}
	}
	return r.data.ProjectBinding(platform, chatType, chatID)
}

// Resolver returns the router's shared KeyResolver instance, or nil if
// none was injected via RouterConfig.Resolver. Downstream consumers
// (dispatch.NewDispatcher, server.Hub, upstream wiring) should prefer
// this accessor over constructing their own KeyResolver from the same
// (Agents, ProjectMgr) inputs — when the prior pattern was practiced
// across all 4 historical construction sites, agent-config edits to one
// resolver did not propagate to the others, producing the silent drift
// documented in R237-ARCH-12 (#604). A non-nil return is safe for
// concurrent reads: KeyResolver is immutable post-construction.
func (r *Router) Resolver() *KeyResolver {
	if r == nil {
		return nil
	}
	return r.resolver
}
