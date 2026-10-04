// Package session — routing.go
//
// KeyResolver centralises the (session key, AgentOpts) derivation used across
// dispatch / server / upstream. Aliasing-safe handling of AgentOpts.ExtraArgs
// is an internal invariant (docs/rfc/key-resolver.md §2.2). PlannerDataSource /
// ProjectBinding are aliased from the neutral leaf internal/projectapi (#1373)
// so session never imports project.
package session

import (
	"slices"
	"strings"

	"github.com/naozhi/naozhi/internal/projectapi"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// SanitisePlannerPromptForSpawn re-validates a planner prompt at the spawn
// boundary; see sessionview.SanitisePlannerPromptForSpawn.
func SanitisePlannerPromptForSpawn(prompt, projectName string) string {
	return sessionview.SanitisePlannerPromptForSpawn(prompt, projectName)
}

// PlannerDataSource abstracts the project-layer data KeyResolver needs. All
// methods return fully-snapshot'd values so callers can treat them as pure
// reads. Aliased from internal/projectapi (#1373).
type PlannerDataSource = projectapi.DataSource

// ProjectBinding is the minimal projection session needs. Populated by the
// project-package adapter via EffectivePlannerModel / EffectivePlannerPrompt,
// so the Resolver does NOT re-implement those precedence rules.
type ProjectBinding = projectapi.ProjectBinding

// KeyResolver derives a (session key, AgentOpts) pair for a dispatch context,
// encoding the project-binding precedence (general → planner, non-general →
// workspace-only) and ExtraArgs aliasing safety as internal invariants.
//
// The zero value is not usable; construct via NewKeyResolver.
type KeyResolver struct {
	defaults    map[string]AgentOpts      // agentID -> base opts
	data        PlannerDataSource         // nil → project feature disabled
	cronProfile func(jobID string) string // nil → cron keys resolve to ""
}

// NewKeyResolver constructs a resolver. data may be nil to disable
// project-aware routing (no chat is ever project-bound).
func NewKeyResolver(defaults map[string]AgentOpts, data PlannerDataSource) *KeyResolver {
	return &KeyResolver{defaults: defaults, data: data}
}

// WithCronAccessProfile returns a copy of r whose AccessProfileForKey asks fn
// for a cron key's profile (job ID → the profile its runs spawn with). r is
// left unchanged, so a resolver already shared stays immutable.
func (r *KeyResolver) WithCronAccessProfile(fn func(jobID string) string) *KeyResolver {
	cp := *r
	cp.cronProfile = fn
	return &cp
}

// ResolveForChat is the "chat-view" path: given IM chat coordinates and
// agentID, return the routed key and merged opts (docs/rfc/key-resolver.md
// §3.3): unbound chat → defaults[agentID] + IM key; bound + non-general →
// overlay Workspace only, Exempt explicitly false; bound + "general" → overlay
// Workspace / Model / Prompt, Exempt = true, planner key.
//
// The planner prompt goes into AgentOpts.SystemPrompt — NOT ExtraArgs, where
// cli.deniedExtraFlags would strip it (#2493). Every return path clones
// ExtraArgs so a caller appending to the returned slice cannot mutate
// r.defaults[agentID].ExtraArgs when cap > len.
func (r *KeyResolver) ResolveForChat(platform, chatType, chatID, agentID string) (key string, opts AgentOpts) {
	base := r.defaults[agentID] // zero-value safe
	base.ExtraArgs = slices.Clone(base.ExtraArgs)

	if r.data == nil {
		return SessionKey(platform, chatType, chatID, agentID), base
	}

	b := r.data.ProjectBinding(platform, chatType, chatID)
	if !b.Bound {
		return SessionKey(platform, chatType, chatID, agentID), base
	}

	if agentID != "general" {
		// Workspace only; no planner model/prompt. Exempt explicitly false so
		// stale defaults cannot promote this session to exempt.
		base.Workspace = b.WorkspaceDir
		base.Exempt = false
		// Backend + access profile ARE inherited: a project pinned to a personal
		// 1P account must not silently run non-general agents on the company
		// Bedrock default (RFC project-access-profile P2-2). Only override when
		// the project pins a value.
		if b.Backend != "" {
			base.Backend = b.Backend
		}
		if b.AccessProfile != "" {
			base.AccessProfile = b.AccessProfile
		}
		return SessionKey(platform, chatType, chatID, agentID), base
	}

	// general agent + bound project ⇒ planner (chat-view).
	base.Exempt = true
	base.Workspace = b.WorkspaceDir
	if b.Backend != "" {
		base.Backend = b.Backend
	}
	// The planner's account and backend come from the project alone, never
	// from defaults["general"]: ResolveForPlannerKey (restart, resume) cannot
	// see general's, and one key must not change either by spawn path.
	base.AccessProfile = b.AccessProfile
	base.DefaultBackend = ""
	if b.PlannerModel != "" {
		base.Model = b.PlannerModel
	}
	// Spawn-boundary re-validation (see SanitisePlannerPromptForSpawn).
	if pp := sessionview.SanitisePlannerPromptForSpawn(b.PlannerPrompt, b.Name); pp != "" {
		// `base` is a value copy, so this never mutates r.defaults[agentID].
		base.SystemPrompt = JoinSystemPrompts(base.SystemPrompt, pp)
	}
	return plannerKeyFor(b.Name), base
}

// ResolveForPlannerKey is the "planner-view" path used by administrative
// restart flows: from a project name, return the planner key and opts.
// Deliberately does NOT inherit from defaults["general"]: it starts from blank
// opts and layers only project configuration (docs/rfc/key-resolver.md §2.2).
// Returns ok=false when the project cannot be found; callers must NOT fall back
// to chat-view behaviour.
func (r *KeyResolver) ResolveForPlannerKey(projectName string) (key string, opts AgentOpts, ok bool) {
	if r.data == nil {
		return "", AgentOpts{}, false
	}
	b, found := r.data.ProjectByName(projectName)
	if !found {
		return "", AgentOpts{}, false
	}
	opts = AgentOpts{
		Exempt:        true,
		Workspace:     b.WorkspaceDir,
		Model:         b.PlannerModel,
		Backend:       b.Backend,
		AccessProfile: b.AccessProfile,
	}
	// Same spawn-boundary check as ResolveForChat: b.PlannerPrompt may be a
	// stale value cached from a prior disk reload.
	if pp := sessionview.SanitisePlannerPromptForSpawn(b.PlannerPrompt, b.Name); pp != "" {
		opts.SystemPrompt = pp
	}
	return plannerKeyFor(b.Name), opts, true
}

// ResolveForKey is the "key-resume" path: given an existing key from
// sessions.json or dashboard WS subscribe, return the AgentOpts for re-spawn.
//
//   - planner key → ResolveForPlannerKey; ok reflects project existence.
//   - other reserved namespaces (cron: / scratch:) → ok=false; caller routes
//     to the namespace's dedicated path.
//   - IM 4-segment key → ok=true, opts = defaults[agentID]. Deliberately does
//     NOT overlay workspace or consult the project binding: resume has no fresh
//     chat context, so a binding lookup would produce stale overrides (RFC §4.5).
//   - malformed → ok=false.
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

// AccessProfileForKey returns the access-profile ID a key resolves to ("" =
// global default). Used by the remote-dispatch gate: a session bound to a
// non-default profile MUST NOT be dispatched to a remote node, because the env
// overlay (and any *_FILE secret) is host-local and never crosses the
// reverse-RPC wire — the remote would silently spawn on the wrong account (RFC
// project-access-profile §4.5). The result covers every profile ResolveForChat
// or ResolveForKey can put in opts.AccessProfile: a project pin, else the
// agent's own (defaults[agentID]; a planner has only the project pin). A cron
// key resolves through WithCronAccessProfile's lookup. Returns "" for other
// reserved namespaces / malformed keys, which the gate treats as "remote OK".
func (r *KeyResolver) AccessProfileForKey(key string) string {
	if r == nil {
		return ""
	}
	if isPlannerKey(key) {
		_, opts, _ := r.ResolveForPlannerKey(plannerNameFromKey(key))
		return opts.AccessProfile
	}
	if IsCronKey(key) {
		if r.cronProfile == nil {
			return ""
		}
		return r.cronProfile(sessionkey.CronJobIDFromKey(key))
	}
	if IsReservedNamespace(key) {
		return ""
	}
	parts := strings.SplitN(key, ":", 4)
	if len(parts) != 4 {
		return ""
	}
	// ResolveForKey deliberately skips the project binding (§4.5), so read it
	// directly: a non-general project-bound session still carries the profile.
	if r.data != nil {
		if b := r.data.ProjectBinding(parts[0], parts[1], parts[2]); b.Bound && b.AccessProfile != "" {
			return b.AccessProfile
		}
	}
	return r.defaults[parts[3]].AccessProfile
}

// KeyForChat is the key-only variant for callers that do not need opts (e.g.
// /stop, /new). Project-bound chats with agentID=="general" get the planner key.
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
// slash-command paths read the bound project from the same snapshot the
// session-key derivation uses; a separate *project.Manager read could race a
// concurrent BindChat / UnbindAllChat within one handler (#648). Returns
// ProjectBinding{Bound: false} when no data source is wired or the chat is
// unbound; callers branch on b.Bound.
func (r *KeyResolver) ProjectBindingForChat(platform, chatType, chatID string) ProjectBinding {
	if r.data == nil {
		return ProjectBinding{}
	}
	return r.data.ProjectBinding(platform, chatType, chatID)
}

// Resolver returns the router's shared KeyResolver, or nil if none was
// injected via RouterConfig.Resolver. Downstream consumers must use this
// accessor rather than constructing their own from (Agents, ProjectMgr):
// independent resolvers do not see each other's agent-config edits (#604).
// Safe for concurrent reads: KeyResolver is immutable post-construction.
func (r *Router) Resolver() *KeyResolver {
	if r == nil {
		return nil
	}
	return r.resolver
}
