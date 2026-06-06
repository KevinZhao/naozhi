package session

import (
	"log/slog"
	"sync/atomic"
)

// workspaceStore groups the three correlated per-chat workspace-override
// fields (Router P1 facet, #383). It is a value field on Router, carries NO
// lock of its own, and is read/written ONLY under Router.mu — the lock
// topology is unchanged (RFC §3 candidate A: single r.mu retained).
//
// overrides stores per-chat workspace overrides.
// Key format: "platform:chatType:chatID" (3-segment chat key —
// distinct from the 4-segment session key used in r.ss.sessions).
//
// Two-key invariant: every chatKey present in sessionsByChat may
// have an overrides entry; ResetChat clears both maps.
// SetWorkspace creates only the override entry (no session yet),
// and Reset(key)/evictOldest must NOT touch this map — it is
// driven by user intent (SetWorkspace) rather than the session
// lifecycle.
type workspaceStore struct {
	// 读写: core (init/load), lifecycle (SetWorkspace/GetWorkspace), cleanup (save), discovery (Takeover), workspace (resolveWorkspaceLocked/WorkspaceRoots)
	overrides map[string]string
	// 读写: lifecycle (ResetChat / RenameSession), discovery (Takeover), cleanup (saveIfDirty consume), workspace (SetWorkspace mutation)
	dirty bool // true when workspace overrides changed since last save
	// 读写: lifecycle (ResetChat / RenameSession), discovery (Takeover), cleanup (snapshot/check during save), workspace (SetWorkspace mutation)
	gen atomic.Uint64 // increments on each ws-override mutation, mirrors storeGen pattern
}

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
	if _, existing := r.wsStore.overrides[chatKey]; !existing && len(r.wsStore.overrides) >= maxWorkspaceOverrides {
		slog.Warn("workspaceOverrides at capacity; dropping override",
			"chat_key", chatKey, "cap", maxWorkspaceOverrides)
		return
	}
	r.wsStore.overrides[chatKey] = path
	r.wsStore.dirty = true
	r.wsStore.gen.Add(1)
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
	if ws, ok := r.wsStore.overrides[chatKey]; ok {
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
	seen := make(map[string]struct{}, len(r.wsStore.overrides)+1)
	out := make([]string, 0, len(r.wsStore.overrides)+1)
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
	for _, ws := range r.wsStore.overrides {
		add(ws)
	}
	return out
}
