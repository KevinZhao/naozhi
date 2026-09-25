package cron

import (
	"path/filepath"

	"github.com/naozhi/naozhi/internal/costledger"
)

// appendLedger writes the run's cost entries: a sandbox run contributes its
// receipt, a local run the session-side increment (one USD row with the
// per-model drill-down plus one row per backend metering unit). cron is the
// run owner for both, so the session layer does not also write them.
func (s *Scheduler) appendLedger(rc runCtx, out runOutcome) {
	if s.ledger == nil || !s.ledger.Enabled() {
		return
	}
	base := costledger.Entry{
		JobID:     rc.job.ID,
		RunID:     rc.runID,
		Workspace: workspaceLabel(rc.snap.workDir),
		Backend:   rc.job.Backend,
	}
	if base.Backend == "" {
		base.Backend = "claude"
	}
	if out.sandboxMeta != nil {
		if out.sandboxMeta.CostUSD > 0 {
			e := base
			e.Source, e.Kind, e.Unit, e.Amount = costledger.SourceCronSandbox, costledger.KindReceipt, costledger.UnitUSD, out.sandboxMeta.CostUSD
			e.Models, e.Basis = out.sandboxMeta.Models, out.sandboxMeta.Basis
			s.ledger.Append(e)
		}
		return
	}
	inc := out.costInc
	if inc.USD > 0 || len(inc.Models) > 0 {
		e := base
		e.Source, e.Kind, e.Unit = costledger.SourceCronLocal, costledger.KindTurn, costledger.UnitUSD
		e.Amount, e.Basis, e.Models = inc.USD, inc.Basis, inc.Models
		if e.Basis == costledger.BasisNone && inc.USD > 0 {
			e.Basis = costledger.BasisList
		}
		s.ledger.Append(e)
	}
	for u, v := range inc.Metered {
		e := base
		e.Source, e.Kind, e.Unit, e.Amount = costledger.SourceCronLocal, costledger.KindMetering, u, v
		s.ledger.Append(e)
	}
}

// workspaceLabel keeps only the directory name so ledger rows carry no
// absolute paths.
func workspaceLabel(workDir string) string {
	if workDir == "" {
		return ""
	}
	b := filepath.Base(workDir)
	if b == "." || b == string(filepath.Separator) {
		return ""
	}
	return b
}
