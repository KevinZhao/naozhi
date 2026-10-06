package metrics

// Spawn-gate observability (#2532): every gate that drops or ignores configured
// input (argv, env filter, capabilities, config load, the session store read
// guard) reports here via spawndiag.Emit, which cli.EmitSpawnDiags re-exports.

// SpawnDiagTotal counts spawn-gate rejections. Labels: layer and action, the
// values documented on spawndiag.Diag. The key itself is NOT a label: unknown
// config keys are operator-typed strings, and one typo per restart would be
// unbounded cardinality. The key is in the log line and in `naozhi config check`.
// spawndiag.Emit dedups per scope+layer+key outside the "config" scope, so
// this reads "distinct ineffective configs observed since process start" — the
// 30s shim-reconcile heartbeat re-deriving the same argv does not inflate it.
// Labeled-only (bare wire name, no ByBackend/By* Go suffix, #2243).
var SpawnDiagTotal = NewLabeledCounter("naozhi_spawn_diag_total", "layer", "action")

// RecordSpawnDiag increments the spawn-gate rejection counter.
func RecordSpawnDiag(layer, action string) {
	SpawnDiagTotal.Add(1, layer, action)
}
