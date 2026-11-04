package notify

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
)

// Source rules (§15.1).
const (
	// Interval is the source check period and the "notify" loop's beat period.
	Interval = time.Minute
	// AskAfter is how long a session waits for the user before it becomes
	// a question.
	AskAfter = 10 * time.Minute
	// DiskLow is the free fraction of the state root's volume below which
	// the disk is a notice.
	DiskLow = 0.10
)

// loopName is this pipeline's heartbeat; the health loop watches it.
const loopName = "notify"

// beatSource is the one item for every stale loop but this one.
const beatSource = "beat:loops"

// gatewaySources are the gateway's own level-triggered source prefixes, judged
// at every check; unitSources those judged once the health machine has
// reconciled. Check dates all of them itself (condSince), since a restart
// restarts the health machine's Since.
var (
	gatewaySources = []string{"beat:", "disk"}
	unitSources    = []string{"waiting:", "stalled:"}
)

// Health is the health state machine (monitor/health.Machine).
type Health interface {
	Snapshot() health.Snapshot
	Subscribe(fn func(health.Event)) (cancel func())
}

type healthEvent = health.Event

// SetHealth binds the health state machine. Call it before Run.
func (n *Notifier) SetHealth(h Health) { n.health = h }

// Run evaluates the sources every Interval until ctx ends, beating the
// "notify" loop, and turns the health machine's transitions and systemic
// faults into items as they come.
func (n *Notifier) Run(ctx context.Context) {
	hb := n.cfg.Beats.Register(loopName, Interval)
	if n.health != nil {
		cancel := n.health.Subscribe(func(ev health.Event) {
			select {
			case n.events <- ev:
			default:
				slog.Warn("notify: health event dropped, queue full")
			}
		})
		defer cancel()
	}
	if n.crashed {
		// Each abnormal exit is its own item, even with an earlier one open.
		n.Add(Item{Kind: KindNotice, Level: LevelTimely, Source: "crash:" + n.cfg.Clock.Now().Format(time.RFC3339),
			Title: "naozhi 上次异常退出", Why: "上次运行没有正常结束，已重新启动"})
	}
	n.safely("check", n.Check)
	tick := time.NewTicker(Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			hb.Beat()
			n.safely("check", n.Check)
		case ev := <-n.events:
			n.safely("health event", func() { n.HandleHealth(ev) })
		}
	}
}

func (n *Notifier) safely(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("notify: "+what+" panicked", "panic", r)
		}
	}()
	fn()
}

// condition is one gateway condition seen at a check.
type condition struct {
	item Item
	// since is when the condition began; an item the user closed after it
	// is not opened again until the condition clears.
	since time.Time
}

// Check evaluates the level-triggered sources: sessions waiting on the user,
// stalled workflows, stale loops and a low disk. Items whose condition
// cleared close themselves, and held uncapped items go out once allowed.
func (n *Notifier) Check() {
	now := n.cfg.Clock.Now()
	var conds []condition
	var keep []string
	// Prefixes this check judged; an open auto-close item under one whose
	// source is not among conds has cleared.
	judged := slices.Clone(gatewaySources)
	if n.health != nil {
		if snap := n.health.Snapshot(); !snap.At.IsZero() {
			judged = append(judged, unitSources...)
			var c []condition
			c, keep = n.unitConditions(snap, now)
			conds = append(conds, c...)
		}
	}
	// Loops stalled together (a frozen gateway stops them all) are one item,
	// closed once every loop beats again. A loop stalling that was not
	// before begins the condition anew: an item the user closed opens again.
	stale := slices.DeleteFunc(n.cfg.Beats.Stale(), func(name string) bool { return name == loopName })
	if slices.ContainsFunc(stale, func(name string) bool { return !slices.Contains(n.staleLoops, name) }) {
		delete(n.condSince, beatSource)
	}
	n.staleLoops = stale
	if len(stale) > 0 {
		conds = append(conds, n.gatewayCondition(now, Item{Kind: KindError, Level: LevelUrgent, Source: beatSource,
			Title: "关键循环停了：" + strings.Join(stale, "、"), Why: "心跳超时，/health 已降级"}))
	}
	if n.cfg.DiskFree != nil {
		if free, err := n.cfg.DiskFree(); err != nil {
			slog.Debug("notify: disk check failed", "err", err)
			judged = slices.DeleteFunc(judged, func(p string) bool { return p == "disk" })
		} else if free < DiskLow {
			conds = append(conds, n.gatewayCondition(now, Item{Kind: KindNotice, Level: LevelTimely, Source: "disk",
				Title: "磁盘空间不足", Why: fmt.Sprintf("状态目录所在磁盘只剩 %.0f%%", free*100)}))
		}
	}
	active := make(map[string]bool, len(conds)+len(keep))
	for _, c := range conds {
		active[c.item.Source] = true
	}
	for _, src := range keep {
		active[src] = true
	}
	for src := range n.condSince {
		if !active[src] && underAny(src, judged) {
			delete(n.condSince, src)
		}
	}
	n.apply(n.settle(conds, active, judged))
}

// settle closes the open auto-close items whose condition cleared, opens
// the conditions not open yet, nor closed by the user since they began,
// retitles the open ones whose title changed (more loops stale), and pushes
// the held items that may go out now.
func (n *Notifier) settle(conds []condition, active map[string]bool, judged []string) effects {
	n.mu.Lock()
	defer n.mu.Unlock()
	fx := n.releaseLocked(n.cfg.Clock.Now())
	var closed []journalLine
	for i := range n.items {
		it := &n.items[i]
		if it.State == StateOpen && it.AutoClose && !active[it.Source] && underAny(it.Source, judged) {
			closed = append(closed, n.closeLocked(i, ClosedByAuto, ""))
		}
	}
	if len(closed) > 0 {
		fx.merge(n.changedLocked(closed...))
	}
	for _, c := range conds {
		if n.dismissedLocked(c.item.Source, c.since) {
			continue
		}
		c.item.AutoClose = true
		if it, opened, f := n.addLocked(c.item); opened {
			fx.merge(f)
		} else if it.Title != c.item.Title {
			n.items[n.indexLocked(it.ID)].Title = c.item.Title
			fx.merge(n.changedLocked())
		}
	}
	return fx
}

// unitConditions are the sessions waiting on the user past AskAfter and the
// stalled workflows and background tasks. Units without a workspace are only
// counted (§3.8).
// keep are the sources whose items stay open without being conditions: a
// session still waiting (its question stays open until answered, even when
// a restart dated its wait anew) and units whose state is Unknown, as right
// after a restart.
func (n *Notifier) unitConditions(snap health.Snapshot, now time.Time) (out []condition, keep []string) {
	for _, u := range snap.Units {
		if u.Workspace == "" {
			continue
		}
		name := n.name(u.ID)
		switch u.State {
		case health.Waiting:
			src := "waiting:" + u.ID
			keep = append(keep, src)
			if since := n.unitSince(src, u.Since); now.Sub(since) >= AskAfter {
				out = append(out, condition{since: since, item: Item{Kind: KindQuestion, Level: LevelTimely,
					Unit: u.ID, WS: u.Workspace, Source: src,
					Title: name + " 在等你回复", Why: fmt.Sprintf("有提问等你回答超过 %d 分钟", int(AskAfter/time.Minute))}})
			}
		case health.Stalled:
			src := "stalled:" + u.ID
			out = append(out, condition{since: n.unitSince(src, u.Since), item: Item{Kind: KindNotice, Level: LevelTimely,
				Unit: u.ID, WS: u.Workspace, Source: src,
				Title: name + " 停滞", Why: fmt.Sprintf("工作流或后台任务 %d 分钟没有新进展", int(health.StallAfter/time.Minute))}})
		case health.Unknown:
			keep = append(keep, "waiting:"+u.ID, "stalled:"+u.ID)
		}
	}
	return out, keep
}

// unitSince dates a unit condition by the earlier of the unit's Since and
// what this run already holds for it: when an earlier check saw it, or the
// item the user closed before a restart (seedDismissed).
func (n *Notifier) unitSince(src string, since time.Time) time.Time {
	if s, ok := n.condSince[src]; ok && s.Before(since) {
		return s
	}
	n.condSince[src] = since
	return since
}

// gatewayCondition dates a gateway condition from when a check first saw it.
func (n *Notifier) gatewayCondition(now time.Time, it Item) condition {
	since, ok := n.condSince[it.Source]
	if !ok {
		since = now
		n.condSince[it.Source] = since
	}
	return condition{item: it, since: since}
}

// seedDismissed dates the level-triggered conditions whose latest item the
// user closed from that item, so a restart does not reopen it while the
// condition still holds; the first check judging it drops the date if it
// cleared.
func (n *Notifier) seedDismissed() {
	for _, it := range n.items {
		if !underAny(it.Source, gatewaySources) && !underAny(it.Source, unitSources) {
			continue
		}
		if it.State == StateClosed && it.ClosedBy == ClosedByUser {
			n.condSince[it.Source] = it.Created
		} else {
			delete(n.condSince, it.Source)
		}
	}
}

// dismissedLocked reports whether the user closed source's latest item after
// its condition began: the condition has not cleared since.
func (n *Notifier) dismissedLocked(source string, since time.Time) bool {
	for i := len(n.items) - 1; i >= 0; i-- {
		if it := n.items[i]; it.Source == source {
			return it.State == StateClosed && it.ClosedBy == ClosedByUser && !it.ClosedAt.Before(since)
		}
	}
	return false
}

func underAny(source string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(source, p) {
			return true
		}
	}
	return false
}

// HandleHealth turns a health event into an item: a unit that failed is an
// error, and a systemic fault an urgent one; the user closes both. A failure
// that is part of a systemic fault gets no item of its own, so the fault is
// pushed once.
func (n *Notifier) HandleHealth(ev health.Event) {
	switch {
	case ev.Fault != nil && !ev.Fault.Resolved:
		f := ev.Fault
		names := make([]string, 0, len(f.Units))
		for _, id := range f.Units {
			names = append(names, n.name(id))
		}
		n.Add(Item{Kind: KindError, Level: LevelUrgent, Source: "systemic:" + string(f.Class),
			Title: "系统性故障：" + ClassLabel(f.Class),
			Why:   fmt.Sprintf("%d 个单元出现同一类错误：%s", len(f.Units), strings.Join(names, "、"))})
	case ev.Transition != nil && ev.Transition.Unit.State == health.Failed:
		u := ev.Transition.Unit
		if n.inFault(u, ev.At) {
			return
		}
		why := "失败"
		if u.ErrClass != health.ErrNone {
			why += " · " + ClassLabel(u.ErrClass)
		}
		n.Add(Item{Kind: KindError, Level: LevelTimely, Unit: u.ID, WS: u.Workspace, Source: "failed:" + u.ID,
			Title: n.name(u.ID) + " 失败", Why: why})
	}
}

// inFault reports whether u's failure merges into a systemic fault: at least
// health.FaultMin units failed with its class within health.FaultWindow.
func (n *Notifier) inFault(u health.Unit, at time.Time) bool {
	if u.ErrClass == health.ErrNone || u.ErrClass == health.ErrOther || n.health == nil {
		return false
	}
	c := 0
	for _, o := range n.health.Snapshot().Units {
		if o.State == health.Failed && o.ErrClass == u.ErrClass && at.Sub(o.Since) <= health.FaultWindow {
			c++
		}
	}
	return c >= health.FaultMin
}

// WatchPeer is the health loop's check on this pipeline (§3.8: a stopped
// loop cannot report itself). Called after each reconcile, it pushes once,
// straight to the exits, when the "notify" loop has gone stale; the inbox
// lock may be what the loop is stuck on, so it is not taken.
func (n *Notifier) WatchPeer() {
	if !slices.Contains(n.cfg.Beats.Stale(), loopName) {
		n.peerAlarmed.Store(false)
		return
	}
	if n.peerAlarmed.Swap(true) {
		return
	}
	now := n.cfg.Clock.Now()
	if now.UnixNano() < n.pausedUntil.Load() {
		n.record("", notifyData{Event: "alarm", Level: LevelUrgent, Reason: "paused"})
		return
	}
	n.record("", notifyData{Event: "alarm", Level: LevelUrgent})
	go n.deliver(Push{ID: "alarm", Kind: KindError, Level: LevelUrgent, Title: "推送管线停了", Body: "notify 循环心跳超时，新的“等你”不会再推送"})
}

func (n *Notifier) name(unit string) string {
	if n.cfg.Name != nil {
		if s := n.cfg.Name(unit); s != "" {
			return s
		}
	}
	return unit
}

// ClassLabel is an error class as the user reads it.
func ClassLabel(c health.ErrorClass) string {
	switch c {
	case health.ErrAuth:
		return "凭证 / 权限拒绝"
	case health.ErrRegion:
		return "地区限制"
	case health.ErrRateLimit:
		return "限流"
	case health.ErrNetwork:
		return "网络 / DNS"
	case health.ErrCredentialExpired:
		return "凭证过期"
	case health.ErrDiskFull:
		return "磁盘满"
	case health.ErrRefusal:
		return "模型拒答"
	case health.ErrOther:
		return "其他"
	}
	return cmp.Or(string(c), "其他")
}
