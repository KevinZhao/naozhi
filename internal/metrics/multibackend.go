// Multi-Backend RFC §10 (Sprint 6a) — backend label dimension on the four
// metrics that genuinely vary by which CLI backend served the request.
// `naozhi_attachment_ref_*_total` deliberately stays unlabeled (RFC §10.2)
// because attachment ref-counting is a property of the .meta sidecar, not
// the backend.
//
// Double-write strategy: every Record* helper bumps BOTH the legacy
// expvar.Int (where it exists) AND the new expvar.Map. This keeps existing
// docs/ops/pprof.md jq queries and dashboards working through a 4-week
// migration window. After the window, the legacy ints can be removed in a
// single follow-up PR — see TODO marker `R222-OBS-MULTIBACKEND-LEGACY`.

package metrics

import "expvar"

// Backend label values. Callers should pass one of these (or a future
// registered profile ID) — the metrics package itself doesn't validate;
// unrecognized values land in the map as-is, which is desirable because
// the label is a debugging aid: a typo there should be visible, not
// silently coerced to "claude".

// CLISpawnTotalByBackend is the labeled vector counterpart to
// CLISpawnTotal. Keys are backend IDs ("claude" | "kiro" | future).
//
// Sum(CLISpawnTotalByBackend) == CLISpawnTotal during the double-write
// window — TestCLISpawnTotal_RecordsLabeledAndLegacy locks this in.
var CLISpawnTotalByBackend = NewLabeledCounter("naozhi_cli_spawn_total_by_backend")

// SessionActive is a NEW gauge introduced by this RFC; no legacy
// unlabeled counterpart existed previously, but for symmetry we still
// register a plain expvar.Int mirror under the canonical name so jq
// queries can read either format. The mirror is the SUM across all
// backends, kept consistent by RecordSessionActive.
var (
	SessionActive          = expvar.NewInt("naozhi_session_active")
	SessionActiveByBackend = NewLabeledGauge("naozhi_session_active_by_backend")
)

// ProtocolRPCErrorTotalByBackend records JSON-RPC errors emitted by a CLI
// backend (currently ACP-only — Claude's stream-json is not RPC-shaped).
// Labels: backend, method, code. method is the RPC method that caused
// the error (e.g. "session/prompt"); code is the JSON-RPC integer code
// stringified ("-32601", "0", etc.).
//
// No legacy unlabeled counterpart — this metric is new with this RFC.
var ProtocolRPCErrorTotalByBackend = NewLabeledCounter("naozhi_protocol_rpc_error_total")

// ACPCancelTotalByBackend counts session/cancel notifications successfully
// written by ACPProtocol.WriteInterrupt. Cancel attempts that hit the
// pre-handshake "no session yet" path return ErrInterruptUnsupported and
// do NOT increment this — they are not real cancels reaching the agent.
//
// Label: backend (always "kiro" today; future ACP-speaking backends will
// share the metric).
var ACPCancelTotalByBackend = NewLabeledCounter("naozhi_acp_cancel_total")

// RecordCLISpawn bumps both the legacy CLISpawnTotal and the labeled
// vector. The single helper at every call site ensures the two cannot
// drift — a partial refactor that drops one but keeps the other would
// otherwise be invisible.
//
// backendID is the canonical backend identifier ("claude" | "kiro" | ...).
// Empty string maps to LabelEmpty in the vector and still bumps the
// legacy counter — useful for tests that haven't wired a real backend.
func RecordCLISpawn(backendID string) {
	CLISpawnTotal.Add(1)
	CLISpawnTotalByBackend.Add(1, backendID)
}

// RecordSessionActive adjusts the active-session gauge. delta is +1
// when a session is registered, -1 when it is removed/evicted. The
// mirrored unlabeled int tracks the cluster-wide total.
//
// Negative drifts: per LabeledGauge.Dec doc, going negative is allowed
// — it surfaces accounting bugs rather than masking them. Bulk paths
// (router.go reconciliation on eviction / cleanup) drive the gauge back
// to the authoritative count via LabeledGauge.Add, so a negative drift
// recovers within at most one cleanup tick.
func RecordSessionActive(backendID string, delta int) {
	if delta == 0 {
		return
	}
	SessionActive.Add(int64(delta))
	if delta > 0 {
		SessionActiveByBackend.Inc(backendID)
	} else {
		SessionActiveByBackend.Dec(backendID)
	}
}

// RecordProtocolRPCError increments the RPC-error vector. Labels:
//
//   - backendID: canonical ID ("kiro", future "gemini")
//   - method:    RPC method that errored ("session/prompt", "initialize",
//     or "" when the parse failure occurred before method extraction)
//   - code:      JSON-RPC error code as a decimal string. Stringifying at
//     the call site is the caller's responsibility; the metrics
//     package does not perform any conversion to keep its dependencies
//     small. Pass "" if no code is available.
//
// All three labels go through clipLabelSegment so an attacker-controlled
// method/code from a malicious agent can't blow up cardinality.
func RecordProtocolRPCError(backendID, method, code string) {
	ProtocolRPCErrorTotalByBackend.Add(1, backendID, method, code)
}

// RecordACPCancel increments the cancel counter. Called from
// ACPProtocol.WriteInterrupt's success path only — see metric doc above.
func RecordACPCancel(backendID string) {
	ACPCancelTotalByBackend.Add(1, backendID)
}
