package session

import "log/slog"

// as maxBackendOverrides (R55-SEC-001): authenticated callers can POST unique
// chat keys to /api/sessions/send and each valid call grows the map by one
// entry with no natural pruning. 1024 comfortably exceeds realistic operator
// usage (one override per chat, typical deployment < 50 chats).
const maxWorkspaceOverrides = 1024

// SetWorkspace sets the working directory override for a chat. Bounded by
// maxWorkspaceOverrides to prevent DoS via unique-chat-key flooding (R58-SEC-H1).
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
	// Allow existing keys to be updated without bumping against the cap;
	// only reject brand-new keys once the limit is hit. Mirrors the
	// SetSessionBackend gating pattern.
	if _, existing := r.workspaceOverrides[chatKey]; !existing && len(r.workspaceOverrides) >= maxWorkspaceOverrides {
		slog.Warn("workspaceOverrides at capacity; dropping override",
			"chat_key", chatKey, "cap", maxWorkspaceOverrides)
		return
	}
	r.workspaceOverrides[chatKey] = path
	r.wsOverridesDirty = true
	r.wsOverridesGen.Add(1)
}

// GetWorkspace returns the effective workspace for a chat key.
func (r *Router) GetWorkspace(chatKey string) string {
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
	if ws, ok := r.workspaceOverrides[chatKey]; ok {
		return ws
	}
	return r.workspace
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
	seen := make(map[string]struct{}, len(r.workspaceOverrides)+1)
	out := make([]string, 0, len(r.workspaceOverrides)+1)
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
	add(r.workspace)
	for _, ws := range r.workspaceOverrides {
		add(ws)
	}
	return out
}
