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
	if ws, ok := r.workspaceOverrides[chatKey]; ok {
		return ws
	}
	return r.workspace
}
