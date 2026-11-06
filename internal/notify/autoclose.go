package notify

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Items that tell the user something happened and close themselves once it
// no longer needs saying: a background task done (until the session is
// opened), a failure recovered, and the upgrade this start made.
const (
	// UpgradeFor is how long the upgrade notice stays open.
	UpgradeFor = 24 * time.Hour
	// viewFor is how long a dashboard's report that the user views a
	// session holds; the dashboard renews it sooner while the session
	// stays open (custody.ViewFor reads the same reports).
	viewFor = time.Minute
)

// Source prefixes of the items here and the failures they close.
const (
	failedPrefix   = "failed:"
	systemicPrefix = "systemic:"
	donePrefix     = "done:"
	upgradePrefix  = "upgrade:"
)

// Start is what this start found: the build's version and the sessions it
// took back from their shims.
type Start struct {
	Version string
	// Shims are the sessions found running in a shim, Reattached the ones
	// taken back.
	Shims, Reattached int
}

// upgradeItem is the item for a start whose version differs from the
// previous start's, prev: a notice, closed after UpgradeFor, while every
// session found in a shim was taken back, else an error the user closes.
func upgradeItem(prev string, s Start) *Item {
	if prev == "" || s.Version == "" || prev == s.Version {
		return nil
	}
	it := &Item{Kind: KindNotice, Level: LevelTimely, Source: upgradePrefix + s.Version, AutoClose: true,
		Title: fmt.Sprintf("已升级到 %s（来自 %s）", s.Version, prev)}
	if s.Shims > 0 {
		it.Why = fmt.Sprintf("会话接回 %d/%d", s.Reattached, s.Shims)
	}
	if s.Reattached < s.Shims {
		it.Kind, it.AutoClose = KindError, false
		it.Why += "，没接回的会话要重新发消息才会继续"
	}
	return it
}

// SetViewing records whether viewer (one dashboard connection) has key
// open, the server's viewing frames: opening a session closes its
// "后台任务跑完了".
func (n *Notifier) SetViewing(key, viewer string, viewing bool) {
	n.mu.Lock()
	if !viewing {
		delete(n.viewers[key], viewer)
		if len(n.viewers[key]) == 0 {
			delete(n.viewers, key)
		}
		n.mu.Unlock()
		return
	}
	if n.viewers[key] == nil {
		n.viewers[key] = map[string]time.Time{}
	}
	n.viewers[key][viewer] = n.cfg.Clock.Now()
	var fx effects
	if i := n.openSourceLocked(donePrefix + key); i >= 0 {
		fx = n.changedLocked(n.closeLocked(i, ClosedByAuto, ""))
	}
	n.mu.Unlock()
	n.apply(fx)
}

// DropViewer forgets viewer, for every session: its connection closed.
func (n *Notifier) DropViewer(viewer string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for key, vs := range n.viewers {
		delete(vs, viewer)
		if len(vs) == 0 {
			delete(n.viewers, key)
		}
	}
}

func (n *Notifier) viewingLocked(key string, now time.Time) bool {
	for _, at := range n.viewers[key] {
		if now.Sub(at) < viewFor {
			return true
		}
	}
	return false
}

func (n *Notifier) openSourceLocked(source string) int {
	for i, it := range n.items {
		if it.State == StateOpen && it.Source == source {
			return i
		}
	}
	return -1
}

// taskDone opens "<名字> 后台任务跑完了" for a session's background task
// that finished while no one was at the session, saying the first line of
// the reply its end got; it closes once the user opens the session. Only the
// user's own sessions count: the Planner's, cron runs, daemons and scout
// runs are not theirs to hear of.
func (n *Notifier) taskDone(d health.TaskDone) {
	u := d.Unit
	if u.Kind != health.KindSession || sessionkey.IsPlannerNSKey(u.ID) || sessionkey.IsCronKey(u.ID) ||
		sessionkey.IsSysKey(u.ID) || sessionkey.IsScoutKey(u.ID) {
		return
	}
	n.mu.Lock()
	viewing := n.viewingLocked(u.ID, n.cfg.Clock.Now())
	n.mu.Unlock()
	if viewing {
		return
	}
	n.Add(Item{Kind: KindNotice, Level: LevelTimely, Unit: u.ID, WS: u.Workspace, Source: donePrefix + u.ID, AutoClose: true,
		Title: n.name(u.ID) + " 后台任务跑完了", Why: cmp.Or(d.Reply, "后台任务 "+d.Task+" 已结束")})
}

// closeSource closes source's open item as cleared, if there is one.
func (n *Notifier) closeSource(source string) {
	n.mu.Lock()
	var fx effects
	if i := n.openSourceLocked(source); i >= 0 && !n.deciding[n.items[i].ID] {
		fx = n.changedLocked(n.closeLocked(i, ClosedByAuto, ""))
	}
	n.mu.Unlock()
	n.apply(fx)
}

// Carried closes the open notices ids as the morning brief listed them:
// the brief is where a notice is read (§15.1).
func (n *Notifier) Carried(ids []string) {
	n.mu.Lock()
	var lines []journalLine
	for _, id := range ids {
		if i := n.indexLocked(id); i >= 0 && n.items[i].State == StateOpen && n.items[i].Kind == KindNotice {
			lines = append(lines, n.closeLocked(i, ClosedByAuto, ""))
		}
	}
	var fx effects
	if len(lines) > 0 {
		fx = n.changedLocked(lines...)
	}
	n.mu.Unlock()
	n.apply(fx)
}

// settledLocked closes what no longer needs saying, at a check: a failure
// whose unit has been idle since after it opened (a turn ended normally), a
// background task's end whose session is gone or started a turn since (the
// user is on it), and the upgrade notice past UpgradeFor or of an earlier
// start. snap is the health table; its zero value (not reconciled yet)
// judges none of the units.
func (n *Notifier) settledLocked(snap health.Snapshot, now time.Time) []journalLine {
	units := make(map[string]health.Unit, len(snap.Units))
	for _, u := range snap.Units {
		units[u.ID] = u
	}
	var lines []journalLine
	for i := range n.items {
		it := n.items[i]
		if it.State != StateOpen || n.deciding[it.ID] {
			continue
		}
		clear := false
		switch {
		case strings.HasPrefix(it.Source, failedPrefix) && !snap.At.IsZero():
			u, ok := units[strings.TrimPrefix(it.Source, failedPrefix)]
			clear = ok && u.State == health.Idle && u.Since.After(it.Created)
		case strings.HasPrefix(it.Source, donePrefix) && !snap.At.IsZero():
			u, ok := units[strings.TrimPrefix(it.Source, donePrefix)]
			clear = !ok || u.State == health.Active && u.Since.After(it.Created)
		case strings.HasPrefix(it.Source, upgradePrefix):
			clear = it.AutoClose && (now.Sub(it.Created) >= UpgradeFor || it.Source != upgradePrefix+n.cfg.Start.Version)
		}
		if clear {
			lines = append(lines, n.closeLocked(i, ClosedByAuto, ""))
		}
	}
	return lines
}
