# Phase 3f Hand-off Report (2026-05-28)

## Status

**Stopped before main implementation** — Phase 3f (SendHandler →
internal/dashboard/send) requires Phase 4b to be properly finished first.

What was done:
- File-level `git mv` of dashboard_send.go + 5 tests to internal/dashboard/send/
- Package + type + method renames (handle\* → Handle\*)
- These changes are NOT in any branch — discarded after the dependency analysis below.

Why stopped: SendHandler holds 5 distinct dependencies on `*server.Hub`'s
unexported method + fields, several of which can't be cleanly satisfied
without first finishing Phase 4b's wshub.Hub vs server.Hub merge.

## Dependencies blocked on Phase 4b completion

dashboard_send.go calls:

| Hub access | Public? | Phase 4b status |
|------------|---------|-----------------|
| `h.hub.TrackSend()` | Public | ✅ exists on both server.Hub and wshub.Hub but **signatures differ**: server returns `(release func(), shuttingDown bool)`; wshub returns `(allowed bool)`. Picking either right now breaks the other. |
| `h.hub.ctx` | Unexported field | Phase 4b dual-Hub state means there are TWO `ctx` fields. |
| `h.hub.BroadcastSessionsUpdate()` | Public | ✅ on both — clean. |
| `h.hub.sessionSend(sendParams{...}, nil)` | **Unexported** method on `*server.Hub` | Phase 4b moved this to `*wshub.Hub.sessionSend` BUT `internal/server/wshub.go` still has its own copy. Calling either from a sub-package requires exporting the method **AND** picking which Hub. |
| `h.hub.allowedRoot` | Unexported field | Same dual-Hub problem. |

Plus `sendParams` and `sendAckStatus` types are server-package private.

## What Phase 4b should finish before Phase 3f

1. **Delete `internal/server/wshub.go`** — its `*Hub` should not exist; all callers should use `*wshub.Hub`.
2. **Reconcile `TrackSend()` signature** — pick the wshub version `(allowed bool)` since it's the newer / cleaner shape, update server callers.
3. **Export `sessionSend` → `SessionSend`** on `*wshub.Hub`, OR introduce a `HubSender` interface in dashboard/send and adapter in server.
4. **Export `sendParams` → `SendParams`**, same for `sendAckStatus`.
5. **Add `Ctx() context.Context` and `AllowedRoot() string` getters** on `*wshub.Hub` (or full Hub method set so dashboard/send can hold the interface, not the struct).

## Phase 3f scope estimate (after 4b prerequisites land)

| Item | Lines |
|------|-------|
| dashboard_send.go (production) | 1511 |
| 5 tests | ~1000 |
| HubSender interface in send sub-package | ~30 |
| ipLimiter / NodeAccessor interface duplication | ~30 |
| mintAnonCookie / uploadOwner / uploadOwnerOrFail helper move | ~150 |
| validateWorkspace closure injection | ~5 |
| server-package wiring (server.go ctor, dashboard.go routes) | ~50 |
| **Total estimate** | **~2800 行** |

Comparable to Phase 2 (project) — 2700 行 — but blocked on the 4b
prerequisites above. With those done, Phase 3f is ~3-4 hours of
mechanical migration plus the careful adapter wiring on the wshub side.

## Recommended order to proceed

1. **Phase 4c (wshub finishing)** — agent_tailer.go (895), eventpush, hub_agent. Drops `internal/server/wshub.go` `*Hub` entirely.
2. **Phase 3f (this work)** — clean migration once `*server.Hub` is gone.
3. **Phase 5 (final reduction)** — Server struct 47 → ≤12 fields + linter fail mode + handle_baseline 8 method handling.

## Alternate path (if Phase 4c is blocked)

Make Phase 3f the trigger for finishing 4b's last loose ends:

- Add `internal/server/hub_compat.go` that re-exports the necessary `*server.Hub` methods (Ctx, AllowedRoot, SessionSend) as public wrappers with type-aliased exported types (SendParams = sendParams). Adapter for `HubSender` interface lives there.
- This lets Phase 3f land BEFORE Phase 4c at the cost of a temporary 50-line compat file in server pkg, removed during 4c.

The compat-file approach is what we did for Phase 3-prep (httputil) when the same problem appeared on a smaller scale; it works but adds a documented exemption to track.

## Why the scope creep in 3f vs other phases

Earlier dashboard sub-package phases (3a auth, 2 project, 1 cron, 3c scratch+memory, 3d agentevents+cli+transcribe, 3e session) never accessed Hub's _internals_ — only the public method surface (`BroadcastSessionsUpdate`, `Router`, etc.) or pure-data deps. Phase 3f is the first sub-package whose handlers depend on Hub's send-pipeline internals (`sessionSend`, `ctx` for timeout, `allowedRoot` for workspace validation).

This is also why Phase 3f was always going to be the highest-risk dashboard split — design v0.4 §6.1 already labelled it "高（含 send 路径）". The dependency analysis here just makes the prerequisites concrete.
