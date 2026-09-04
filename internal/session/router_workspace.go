package session

import "log/slog"

// The per-chat workspace-override facet (#383) lives in
// internal/session/workspacestore (#2495): its fields are private to that
// package, so every access from Router goes through the Store method
// surface and the compiler — not the `// 读写:` inner-field annotations —
// enforces the boundary. The store owns NO lock; every call below happens
// under r.mu (see the workspacestore package doc for the cross-facet
// atomicity requirements of #2342 that keep it there).

// maxWorkspaceOverrides bounds the per-chat override map: authenticated
// callers can POST unique chat keys to /api/sessions/send and each valid call
// grows the map by one entry with no natural pruning. 1024 comfortably exceeds
// realistic operator usage (one override per chat, typically < 50 chats).
const maxWorkspaceOverrides = 1024

// SetWorkspace sets the working directory override for a chat. Bounded by
// maxWorkspaceOverrides to prevent DoS via unique-chat-key flooding.
//
// One-shot dashboard:direct keys are never recycled once their session ends
// (only /clear, /new discard an override), so the map grows toward the cap.
// At cap we EVICT the least-recently-set override that has no live session
// rather than dropping the new write, so a fresh session past 1024 still gets
// its project dir and the DoS bound holds (size never exceeds the cap).
func (r *Router) SetWorkspace(chatKey, path string) {
	// Fail closed on empty chatKey before taking the lock: an override under
	// "" would make GetWorkspace("") return the attacker-supplied path instead
	// of the configured fallback, so a downstream handler passing chatKey
	// through unsanitized would route to an attacker-controlled directory.
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

// resolveWorkspaceLocked is the single chat-level workspace resolution point
// (#883): per-chat override first, router default otherwise. Caller holds r.mu
// (read or write). resolveSpawnParamsLocked layers the opts/resume tiers ON
// TOP of this base rather than re-deriving it.
func (r *Router) resolveWorkspaceLocked(chatKey string) string {
	if ws, ok := r.wsStore.Lookup(chatKey); ok {
		return ws
	}
	return r.defaultCWD
}

// WorkspaceRoots returns the deduplicated set of workspace roots this router
// knows about: the default workspace plus every per-chat override value (used
// by the attachment-gc daemon, docs/rfc/attachment-gc-daemon.md §4.4). Roots
// are returned raw (not symlink-resolved) — the caller normalises + dedupes.
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
