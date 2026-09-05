package metrics

// Spawn-gate observability (#2532): every gate on the spawn pipeline that
// drops or ignores configured input (argv denylist, capability gate,
// deprecated config fields) reports here via cli.EmitSpawnDiags.

// SpawnDiagTotal counts spawn-gate rejections. Labels: layer ("argv-denylist"
// | "caps" | "config-deprecated"), action ("dropped" | "ignored").
// cli.EmitSpawnDiags dedups per scope+layer+key, so this reads "distinct
// ineffective configs observed since process start" — the 30s shim-reconcile
// heartbeat re-deriving the same argv does not inflate it. Labeled-only
// (bare wire name, no ByBackend/By* Go suffix, #2243).
var SpawnDiagTotal = NewLabeledCounter("naozhi_spawn_diag_total")

// RecordSpawnDiag increments the spawn-gate rejection counter.
func RecordSpawnDiag(layer, action string) {
	SpawnDiagTotal.Add(1, layer, action)
}
