package session

import (
	"log/slog"
	"os"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/osutil"
)

// costMarkSlack widens the ledger scan below the store's mtime. Correctness
// does not depend on it: the spend comparison decides.
const costMarkSlack = 10 * time.Minute

// costMarkFallback is how far back the scan reaches when the store's mtime
// is unreadable.
const costMarkFallback = 48 * time.Hour

// costMarkKey identifies a logical session: a deleted and recreated key gets
// a new createdAt, so the old session's marks never match it.
type costMarkKey struct {
	key  string
	born int64
}

// costMarkLocked is the mark of the booking that just advanced s's spend and
// its CLI baseline to cum. Nil when s has no creation stamp. Caller holds
// costMu, so spend and baseline come from the same booking.
func (s *ManagedSession) costMarkLocked(cum float64) *costledger.SessionMark {
	born := s.createdAt.Load()
	if born == 0 {
		return nil
	}
	return &costledger.SessionMark{Spent: loadTotalCost(&s.costSpent), Cum: cum, Born: born}
}

// costMarksSince is where the restore scan starts: a margin below the
// store's last write.
func costMarksSince(storePath string, now time.Time) time.Time {
	fi, err := os.Stat(storePath)
	if err != nil {
		return now.Add(-costMarkFallback)
	}
	since := fi.ModTime().Add(-costMarkSlack)
	if floor := now.Add(-(costledger.MaxQueryDays - 1) * 24 * time.Hour); since.Before(floor) {
		return floor // Scan refuses a wider window
	}
	return since
}

// sessionMarks returns, per logical session, the mark with the highest spend
// among the session rows written since since. Rows are appended outside
// costMu and may land out of order, so the last row is not necessarily the
// newest mark. Nil when the ledger is off.
func (c *costAccounting) sessionMarks(since, now time.Time) map[costMarkKey]costledger.SessionMark {
	if c == nil || !c.ledger.Enabled() {
		return nil
	}
	marks := make(map[costMarkKey]costledger.SessionMark)
	err := c.ledger.Scan(costledger.Query{From: since, To: now.Add(time.Hour)}, func(e costledger.Entry) bool {
		m := e.Mark
		if e.Source != costledger.SourceSession || m == nil || m.Born == 0 || m.Cum < 0 {
			return true
		}
		k := costMarkKey{e.SessionKey, m.Born}
		if cur, ok := marks[k]; !ok || m.Spent > cur.Spent {
			marks[k] = *m
		}
		return true
	})
	if err != nil {
		slog.Warn("cost: ledger marks unreadable; restored sessions keep the stored baseline", "err", err)
		return nil
	}
	return marks
}

// adoptCostMark replaces s's restored spend and baseline with m when m
// records more spend than the store did. The store is saved every
// sessionSaveInterval but a ledger row within about a second of its booking,
// so after a crash a reattached CLI would otherwise difference its next
// result against a stale baseline and book the gap twice (#3518). Spend only
// grows within a logical session, so it orders m against the store. The
// per-model baseline stays unknown, as after any restore.
func (s *ManagedSession) adoptCostMark(m costledger.SessionMark) bool {
	s.costMu.Lock()
	defer s.costMu.Unlock()
	stored := loadTotalCost(&s.costSpent)
	if m.Spent <= stored {
		return false
	}
	slog.Info("cost: restored session's baseline taken from the ledger, which is ahead of the store",
		"key", osutil.SanitizeForLog(s.key, 128), "store_spent", stored, "store_baseline", loadTotalCost(&s.lastCumulativeCost),
		"ledger_spent", m.Spent, "ledger_baseline", m.Cum)
	storeTotalCost(&s.costSpent, m.Spent)
	storeTotalCost(&s.lastCumulativeCost, m.Cum)
	s.lastCumulative = costledger.Cumulative{USD: m.Cum}
	s.modelsBaselineUnknown = true
	return true
}
