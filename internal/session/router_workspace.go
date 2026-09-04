package session

import "log/slog"

// The per-chat workspace-override facet (Router P1, #383) lives in
// internal/session/workspacestore as of #2495 step 1: its fields are private
// to that package, so every access from Router goes through the Store method
// surface and the compiler — not the `// 读写:` inner-field annotations —
// enforces the boundary. The store owns NO lock; every call below happens
// under r.mu (see the workspacestore package doc for why the cross-facet
// atomicity requirements of #2342 / Round-207 SM1 keep it there).

// maxWorkspaceOverrides bounds the per-chat override map. Same rationale
// as maxBackendOverrides (R55-SEC-001): authenticated callers can POST unique
// chat keys to /api/sessions/send and each valid call grows the map by one
// entry with no natural pruning. 1024 comfortably exceeds realistic operator
// usage (one override per chat, typical deployment < 50 chats).
const maxWorkspaceOverrides = 1024

// SetWorkspace sets the working directory override for a chat. Bounded by
// maxWorkspaceOverrides to prevent DoS via unique-chat-key flooding (R58-SEC-H1).
//
// Capacity self-healing (#cwd-overflow): one-shot dashboard:direct keys are
// never recycled once their session ends (only /clear, /new discard an
// override), so the map monotonically grows toward the cap. The old behaviour
// — silently DROP the NEW override once full — meant every freshly created
// session past 1024 fell back to defaultCWD (the workspace root) instead of
// its project dir. We now EVICT the least-recently-set override that has no
// live session instead of rejecting the new write, so the map self-heals and
// the DoS bound still holds (size never exceeds the cap).
func (r *Router) SetWorkspace(chatKey, path string) {
	// R20260527122801-CR-16: reject empty chatKey before taking the lock.
	// An unauthenticated or misrouted dashboard request that reaches this
	// path with chatKey=="" used to silently install an override under the
	// empty-string key — that single slot is harmless on its own, but the
	// pre-check also disarms a class of misuse where every sentinel-keyed
	// caller stomps the same slot, masking the originating call site. More
	// importantly, GetWorkspace("") would then return the attacker-supplied
	// path instead of the configured workspace fallback, so a downstream
	// handler that passes chatKey through unsanitized would route to an
	// attacker-controlled directory. Fail closed here.
	if chatKey == "" {
		slog.Warn("SetWorkspace: empty chatKey rejected",
			"hint", "caller passed unauthenticated or misrouted chat_key — verify upstream auth")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.putWorkspaceOverrideLocked(chatKey, path)
}

// putWorkspaceOverrideLocked installs the override under the capacity
// policy (existing key: update in place; new key at cap: LRU-evict a
// session-less override, else drop). Shared by SetWorkspace and the atomic
// ResetChatAndSetWorkspace path (#2342) so the "is this chat live" predicate
// — the one piece of cross-facet state the eviction needs — is bound in
// exactly one place. Caller holds r.mu. Reports whether the write landed.
func (r *Router) putWorkspaceOverrideLocked(chatKey, path string) bool {
	// "No live session" is len(r.ss.byChat[chatKey])==0, so an active
	// conversation never loses its cwd. Indexing a nil byChat map yields a
	// nil set (len 0), so no nil guard is needed.
	isLive := func(k string) bool { return len(r.ss.byChat[k]) > 0 }
	return r.wsStore.SetBounded(chatKey, path, maxWorkspaceOverrides, isLive)
}

// GetWorkspace returns the effective workspace for a chat key.
func (r *Router) Workspace(chatKey string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveWorkspaceLocked(chatKey)
}

// resolveWorkspaceLocked is the single chat-level workspace resolution
// point (R245-ARCH-32 / #883): per-chat override first, router default
// otherwise. Caller holds r.mu (read or write). Extracted so the priority
// order lives in exactly one place — the spawn-time resolver in
// resolveSpawnParamsLocked layers the additional opts/resume tiers ON TOP
// of this chat-level base rather than re-deriving it independently.
func (r *Router) resolveWorkspaceLocked(chatKey string) string {
	if ws, ok := r.wsStore.Lookup(chatKey); ok {
		return ws
	}
	return r.defaultCWD
}

// WorkspaceRoots returns the deduplicated set of workspace roots this
// router knows about: the default workspace plus every per-chat
// override value. The attachment-gc daemon unions this with bound
// project paths to find every <root>/.naozhi/attachments dir to sweep
// (docs/rfc/attachment-gc-daemon.md §4.4). Roots are returned raw (not
// symlink-resolved) — the caller normalises + dedupes across sources.
func (r *Router) WorkspaceRoots() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{}, r.wsStore.Len()+1)
	out := make([]string, 0, r.wsStore.Len()+1)
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	add(r.defaultCWD)
	r.wsStore.Range(func(_, ws string) { add(ws) })
	return out
}
