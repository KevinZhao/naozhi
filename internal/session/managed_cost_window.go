package session

import (
	"log/slog"

	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/osutil"
)

// BeginCostWindow opens the session's cost window for a run owner that writes
// its own ledger entry (a cron run, docs/rfc/cost-ledger.md §5.0): until
// EndCostWindow, the spend this session's results report is collected for the
// owner instead of written as session rows. Process-end partials and spend
// forwarded from a replaced session are never collected; they keep their rows.
func (s *ManagedSession) BeginCostWindow() {
	s.costMu.Lock()
	reopened := s.costWindow != nil
	s.costWindow = &costledger.Totals{}
	s.costMu.Unlock()
	if reopened {
		slog.Warn("cost: cost window opened while already open; the earlier window's spend goes unbooked",
			"session", osutil.SanitizeForLog(s.key, 128))
	}
}

// EndCostWindow closes the window and returns the spend collected in it, which
// the owner must book. Every reading lands either here or in a session row,
// decided under the same costMu section that adds it. Zero when no window is
// open, so a second call is harmless.
func (s *ManagedSession) EndCostWindow() costledger.Increment {
	s.costMu.Lock()
	w := s.costWindow
	s.costWindow = nil
	s.costMu.Unlock()
	if w == nil {
		return costledger.Increment{}
	}
	return w.Sub(costledger.Totals{})
}
