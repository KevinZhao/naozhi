# Phase 4c Hand-off Report (2026-05-28)

## Status

**Stopped before main implementation** — the existing
`docs/ops/phase4c-implementation-playbook.md` (188 lines, written at the
end of Phase 4b) assumes Phase 4b moved Register / Unregister / Broadcast /
Send / wsclient code into `internal/wshub/`. **It did not.** Reality check
shows Phase 4b only moved the package skeleton + 49-field Hub struct +
`NewHub`/`Shutdown`. All method bodies are still placeholders.

This means doing "Phase 4c" right now actually requires doing all of Phase
4b's leftover work first, plus Phase 4c. Combined this is the biggest
single migration in the whole server-split:

| File | Production lines |
|------|------------------|
| internal/server/wshub.go | 1107 |
| internal/server/wshub_subscribe.go | 429 |
| internal/server/wshub_send.go | 419 |
| internal/server/wshub_broadcast.go | 414 |
| internal/server/wshub_eventpush.go | 410 |
| internal/server/wshub_upgrade.go | 348 |
| internal/server/wshub_agent.go | 260 |
| internal/server/wshub_eventpush_cache.go | 185 |
| internal/server/wshub_types.go | 58 |
| internal/server/wsclient.go | 474 |
| **Production total** | **4104** |
| 21 wshub_*_test.go + wsclient_test.go | **~2535** |
| **Grand total** | **~6600** |

Plus Phase 4c proper (agent_tailer.go 895 + eventpush extensions). The
"4b-leftover + 4c" combined PR would land near **8000 lines** — **larger
than Phase 1 cron (7400)** and operating on the hot path (broadcast, send,
WS upgrade).

## Reality verification commands

```bash
# Check Phase 4b status — placeholder vs real
grep -c "Phase 4b 实装" internal/wshub/*.go
# Expect 5+ matches if Phase 4b is incomplete (placeholders still mention "Phase 4b 实装")

# Check where Hub.Register has its real body
grep -nE '^func \(h \*Hub\) Register\b' internal/server/*.go internal/wshub/*.go
# If wshub returns an empty stub and server has the full body, Phase 4b is incomplete

# Check where Hub.sessionSend lives
grep -nE '^func \(h \*Hub\) sessionSend\b' internal/server/*.go internal/wshub/*.go
# Same diagnostic
```

Today the diagnostics show:
- `internal/wshub/hub_subscribe.go:16` Register is `_ = c; return nil`
- `internal/server/send.go:177` sessionSend has the real implementation
- `dashboard.go:130` calls `s.hub = NewHub(...)` resolving to `internal/server/wshub.go.NewHub` — production runs the server-package Hub, NOT `internal/wshub.Hub`

## What Phase 4b actually completed

| Item | Status |
|------|--------|
| Sub-package directory skeleton | ✅ created at internal/wshub/ |
| 49-field Hub struct + field-block godoc | ✅ in internal/wshub/hub.go |
| NewHub + Shutdown coordination | ✅ functional |
| HubRouter interface | ✅ moved to wshub package |
| MessageEnqueuer / Auth / CronView interfaces | ✅ in wshub/types.go |
| Hub method placeholders (Register, BroadcastSessionsUpdate, sessionSend, SubscribeAgent) | ✅ as `_ = arg; return nil` stubs |
| **Real method bodies migration** | ❌ NONE done |
| `internal/server/wshub.go` deletion | ❌ all 4104 production lines still in server pkg |

So PR #1430's "Phase 4b" was actually **Phase 4b-skeleton** — what should
have been called Phase 4a stretched, with method bodies deferred.

## Why this matters for Phase 3f

Phase 3f's hand-off ([phase3f-handoff-2026-05-28.md](phase3f-handoff-2026-05-28.md))
correctly identified that SendHandler depends on `*Hub.sessionSend`,
`*Hub.ctx`, `*Hub.allowedRoot` — all unexported on `*server.Hub`. **The
`*server.Hub` struct's existence** is the underlying problem. Once
Phase 4b method bodies actually move to `*wshub.Hub` (which is the public
target), the dashboard sub-packages can hold a proper interface against
that public surface.

## Recommended path forward (3 options)

### Option A: complete Phase 4b properly first (recommended)

Open a `phase4b-real` PR that migrates all 10 wshub*.go file bodies from
server to wshub package. ~6600 line PR but it's mechanical sed-style work
plus careful test migration. After this:
- Phase 4c (agent_tailer + eventpush proper) — ~2000 行
- Phase 3f (SendHandler) — ~2800 行
- Phase 5 (Server reduction) — ~500 行

Total remaining: ~12000 lines across 4 PRs.

### Option B: hub_compat shim path

Land Phase 3f via a temporary `internal/server/hub_compat.go` that
re-exports `sessionSend`, `Ctx()`, `AllowedRoot()` as public wrappers on
`*server.Hub`. Phase 3f migrates against the compat shim. Phase 4b-real
later removes both server.Hub and the shim. Adds a 50-line tracked
exemption.

### Option C: pause and re-plan

Treat Phase 4b/4c/3f as a unified "Hub merge" project, design it
holistically, and run it as one large PR (~10000 lines) under explicit
exemption since the hot-path surfaces shouldn't be split across
incremental PRs. Highest review effort, lowest correctness risk
(everything migrates together so no half-state).

## What I would NOT do

- Open a "Phase 4c" PR that only does agent_tailer + eventpush while leaving
  send/broadcast/subscribe in server package. That PR builds on a fiction
  and contributes to drift between the playbook and reality.
- Try to migrate just send.go (the 704-line file with sessionSend etc)
  separately. Send.go has bidirectional deps with broadcast (eventPushLoop
  reads send queue state) so they need to migrate together.

## Status of dashboard sub-packages

These are clean — Phase 4b's incompleteness doesn't block them because
they only touch Hub's public method surface:

- ✅ Phase 3a auth (#1436 merged)
- ✅ Phase 3b discovery (#1434 merged)
- ✅ Phase 3-prep httputil (#1433 merged)
- ✅ Phase 3c scratch+memory (#1439 merged)
- ✅ Phase 3d agentevents+cli+transcribe (#1440 merged)
- 🟡 Phase 2 project (#1437 open)
- 🟡 Phase 1 cron (#1438 open, stacked on phase2)
- 🟡 Phase 3e session (#1441 open)
- 🚫 Phase 3f send — needs Phase 4b-real first ([handoff](phase3f-handoff-2026-05-28.md))

Six dashboard sub-packages migrated, 8 PRs landed (5 merged + 3 in queue).
The remaining hot-path migration is the genuine architectural risk — it
deserves its own design pass rather than tactical incremental PRs.
