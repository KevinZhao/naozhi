// Backend label dimension (Multi-Backend RFC §10) on the metrics that vary by
// which CLI backend served the request. `naozhi_attachment_ref_*_total` stays
// unlabeled (RFC §10.2): ref-counting is a property of the .meta sidecar.
// Record* helpers double-write the legacy expvar.Int (where one exists) and
// the labeled map so existing docs/ops/pprof.md jq queries keep working.

package metrics

import "expvar"

// Backend label values are not validated here: an unrecognized value lands in
// the map as-is so a typo is visible rather than coerced to "claude".

// CLISpawnTotalByBackend is the labeled counterpart to CLISpawnTotal, keyed
// by backend ID. Sum over backends == CLISpawnTotal.
var CLISpawnTotalByBackend = NewLabeledCounter("naozhi_cli_spawn_total_by_backend")

// SessionActive is the unlabeled mirror (sum across backends) of
// SessionActiveByBackend, kept consistent by RecordSessionActive so jq
// queries can read either form.
var (
	SessionActive          = expvar.NewInt("naozhi_session_active")
	SessionActiveByBackend = NewLabeledGauge("naozhi_session_active_by_backend")
)

// ProtocolRPCErrorTotal counts JSON-RPC errors from a CLI backend (ACP-only
// today). Labels: backend, method, code (JSON-RPC code as decimal string).
// Labeled-only with no unlabeled mirror, so it keeps the bare wire name and
// omits the `ByBackend` identifier suffix — that suffix is reserved for
// vectors that also carry a `_by_backend` wire alias (#2243).
var ProtocolRPCErrorTotal = NewLabeledCounter("naozhi_protocol_rpc_error_total")

// ACPCancelTotal counts session/cancel notifications written by
// ACPProtocol.WriteInterrupt. Pre-handshake attempts return
// ErrInterruptUnsupported and are not counted. Label: backend. Labeled-only;
// bare wire name for the same reason as ProtocolRPCErrorTotal (#2243).
var ACPCancelTotal = NewLabeledCounter("naozhi_acp_cancel_total")

// RecordCLISpawn bumps both CLISpawnTotal and the labeled vector so the two
// cannot drift. An empty backendID maps to LabelEmpty and still bumps the
// legacy counter.
func RecordCLISpawn(backendID string) {
	CLISpawnTotal.Add(1)
	CLISpawnTotalByBackend.Add(1, backendID)
}

// RecordSessionActive adjusts the active-session gauge (+1 register, -1
// remove/evict) and its unlabeled mirror. Negative drift is allowed (see
// LabeledGauge.Dec); bulk router reconciliation restores the authoritative
// count within one cleanup tick.
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

// RecordProtocolRPCError increments the RPC-error vector. method is "" when
// the parse failure occurred before method extraction; code is the JSON-RPC
// code already stringified by the caller ("" if none). All labels go through
// clipLabelSegment so a malicious agent cannot blow up cardinality.
func RecordProtocolRPCError(backendID, method, code string) {
	ProtocolRPCErrorTotal.Add(1, backendID, method, code)
}

// RecordACPCancel increments ACPCancelTotal; called from
// ACPProtocol.WriteInterrupt's success path only.
func RecordACPCancel(backendID string) {
	ACPCancelTotal.Add(1, backendID)
}
