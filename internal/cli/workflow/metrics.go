package workflow

import "expvar"

// Process-wide counters (docs/ops/pprof.md), summed over every Tracker.
var (
	// untrackedTotal counts workflow tasks refused because maxTracked
	// unsettled workflows were already tracked.
	untrackedTotal = expvar.NewInt("naozhi_cli_workflow_untracked_total")

	// itemsPartialTotal counts snapshots applied with a non-identity field
	// zeroed by a type error.
	itemsPartialTotal = expvar.NewInt("naozhi_cli_workflow_items_partial_total")

	// itemsIdentityTotal counts snapshots dropped (rows kept) because an
	// item or an identity field had the wrong type.
	itemsIdentityTotal = expvar.NewInt("naozhi_cli_workflow_items_identity_total")

	// itemsUnknownTotal counts snapshots and result files holding items of
	// a type the Tracker does not know (ignored).
	itemsUnknownTotal = expvar.NewInt("naozhi_cli_workflow_items_unknown_total")

	// phasesCappedTotal counts snapshots and result files whose phases past
	// maxPhases were dropped.
	phasesCappedTotal = expvar.NewInt("naozhi_cli_workflow_phases_capped_total")
)
