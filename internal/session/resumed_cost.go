package session

import (
	"log/slog"

	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/costledger/cliusage"
)

// resumedBaseline is where a resumed CLI's cumulative cost starts (#3096).
// A fresh process counts from 0, but `claude --resume` restores the
// transcript's last cost-state, so its first reading already carries the
// session's earlier spend. Differencing that against 0 charged the whole
// history to the first turn after every respawn — the same ~$930 three times
// for one session.
type resumedBaseline struct {
	cum costledger.Cumulative
	// known: cum is the total the CLI restores.
	known bool
	// unknown: the CLI may restore a total that could not be read, so the
	// first reading is adopted as the baseline (markCostBaselineUnknown).
	// That turn then charges nothing, which is the side to err on: a missed
	// turn, never a history charged again.
	unknown bool
}

// resumedCostBaseline reads the cost the resumed CLI restores, outside any
// lock (it scans the transcript). A fresh spawn, or a backend that restores
// no cost, starts from 0 as before.
func resumedCostBaseline(claudeDir string, backendDirs map[string]string, res *spawnReservation) resumedBaseline {
	if res.resumeID == "" {
		return resumedBaseline{}
	}
	id := res.backendID
	if id == "" {
		id = "claude"
	}
	p, ok := backendProfile(id)
	if !ok || p.ResumedCost == nil {
		return resumedBaseline{}
	}
	var target string
	if p.ResumeTarget != nil {
		target = p.ResumeTarget(backendDirs[id], claudeDir, res.workspace, res.resumeID)
	}
	if target == "" {
		slog.Warn("cost: resumed session's transcript not locatable; first turn after resume charges nothing",
			"key", res.key, "backend", id)
		return resumedBaseline{unknown: true}
	}
	usd, models, found, err := p.ResumedCost(target, res.resumeID)
	if err != nil {
		slog.Warn("cost: resumed cost baseline unreadable; first turn after resume charges nothing",
			"key", res.key, "backend", id, "err", err)
		return resumedBaseline{unknown: true}
	}
	if !found {
		return resumedBaseline{}
	}
	return resumedBaseline{cum: cliusage.Cumulative(usd, models), known: true}
}

// applyLocked installs the baseline on a freshly installed session. Caller
// holds s.costMu.
func (b resumedBaseline) applyLocked(s *ManagedSession) {
	switch {
	case b.known:
		s.lastCumulative = b.cum
		storeTotalCost(&s.lastCumulativeCost, b.cum.USD)
	case b.unknown:
		s.costBaselineUnknown = true
	}
}
