// Package ccmodels reconciles the Claude model list that Amazon's Builder
// Toolbox wrapper recommends with the lists naozhi and the operator's
// interactive cc actually spawn with.
//
// The toolbox wrapper owns the recommendation: on every launch it writes what
// it considers usable into ~/.claude/settings.json and records that decision in
// ~/.claude/.amzn/state/recommendation-snapshot.json. That snapshot is this
// package's input — Snapshot reads it and derives one Alias per
// "modelOverrides.<alias>" key.
//
// The snapshot is necessary but NOT sufficient, because two failure modes only
// show up when a model is actually invoked:
//
//   - The inference profile exists and is listed, but the caller's IAM role has
//     no bedrock:InvokeModel on it. Selecting such a model fails at turn time.
//   - The alias never reaches its profile. cc resolves a bare family alias to
//     its dated form BEFORE consulting modelOverrides, so an override hung on
//     the bare alias never matches; the alias goes to Bedrock verbatim, gets a
//     400, and cc silently falls back to fallbackModel. The picker row looks
//     healthy and answers questions — with the wrong model.
//
// Deciding those requires probing, which is why Status/Verdict are inputs here
// rather than something this package computes: everything in ccmodels is pure
// and unit-testable, and the subpackage ccprobe does the exec/network work.
//
// BuildPlan turns a Snapshot plus Verdicts into a Plan — the availableModels
// and modelOverrides pair this package owns — and PatchSettings writes that
// pair into a settings document while preserving every other key and the
// document's key order.
//
// Two policies are deliberate and pinned by tests:
//
//   - An unprobed or undecided alias is KEPT. A probe outage (expired
//     credentials, throttling, no network) must not quietly strip an
//     operator's model list.
//   - When a profile id spells an alias differently than the snapshot does
//     (RepairCandidate), that spelling wins. It is the one cc cannot rewrite
//     out from under the override, which turns the silent-fallback bug above
//     into a self-healing substitution instead of a standing trap.
package ccmodels
