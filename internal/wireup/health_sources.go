package wireup

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/bgtask"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cliinfo"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/digest"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/sysession"
	"github.com/naozhi/naozhi/internal/textutil"
)

// askScan is how many trailing events are searched for an AskUserQuestion
// the user has not answered yet; a card further back is not counted.
const askScan = 64

// finishScan is how many trailing events are searched for a background
// task's end and the reply its turn gave.
const finishScan = 256

// HealthSources are the existing state the health monitor derives its units
// from (docs/rfc/planner.md §3.8); nil fields are skipped. Remote and
// Discovered read the server's node and terminal-discovery caches.
type HealthSources struct {
	Router      *session.Router
	Cron        *cron.Scheduler
	Sysession   *sysession.Manager
	Remote      func() (map[string][]map[string]any, map[string]string)
	Discovered  func() []discovery.DiscoveredSession
	EventLogDir string
	// Sidecars fill index.json's purpose and dispatched_by_planner.
	Sidecars *sidecar.Store
	// Scout is the patrol's unit; its run sessions are not units of their
	// own.
	Scout *scout.Scout
	// Digests fill index.json's digest and report (§15.12 ②); nil leaves
	// them out. Observed names the sessions the user opened that the
	// gateway observes, whose purpose index.json says is "chat" with no
	// sidecar saying so; nil names none.
	Digests  *digest.Writer
	Observed func(key string) bool
}

// Collect is health.Config.Collect. Cron jobs and daemons come before the
// sessions they run in, so a merged unit keeps their kind.
func (h HealthSources) Collect(since time.Time) []health.Fact {
	var out []health.Fact
	now := time.Now()
	if h.Cron != nil {
		started := h.Cron.StartedAt()
		for _, jw := range h.Cron.ListAllJobsWithNextRun() {
			_, running := h.Cron.CurrentRun(jw.Job.ID)
			out = append(out, cronFact(jw.Job, running, now, started, since))
		}
	}
	if h.Sysession != nil {
		for _, d := range h.Sysession.Inspector() {
			out = append(out, daemonFact(d))
		}
	}
	if st := h.Scout.Status(); st.Enabled {
		out = append(out, scoutFact(st))
	}
	if h.Router != nil {
		for _, snap := range h.Router.ListSessions() {
			if sessionkey.IsScoutKey(snap.Key) {
				continue
			}
			f := sessionFact(snap, h.Router.SessionFor(snap.Key), h.EventLogDir)
			if h.Sidecars != nil {
				if m, ok := h.Sidecars.Get(snap.Key); ok {
					f.Index.Purpose, f.Index.DispatchedByPlanner = m.Purpose, m.DispatchedByPlanner()
				}
			}
			observed := h.Observed != nil && h.Observed(snap.Key)
			if f.Index.Purpose == "" && observed {
				f.Index.Purpose = sidecar.PurposeChat
			}
			// A session no longer observed keeps its last digest on disk
			// a while, not pointed at: it is not kept current.
			if observed || sessionkey.IsTaskKey(snap.Key) {
				h.indexDigest(f.Index)
			}
			out = append(out, f)
		}
	}
	if h.Remote != nil {
		sessions, status := h.Remote()
		for nodeID, list := range sessions {
			for _, m := range list {
				if f, ok := remoteFact(nodeID, status[nodeID] == "error", m); ok {
					out = append(out, f)
				}
			}
		}
		// A purged node lists no sessions but stays "error".
		for nodeID, st := range status {
			if st == "error" {
				out = append(out, health.Fact{Node: nodeID, NodeDown: true})
			}
		}
	}
	if h.Discovered != nil {
		for _, d := range h.Discovered() {
			out = append(out, externalFact(d))
		}
	}
	return out
}

// indexDigest fills e's digest path and report from the digests. The task
// card reads the report from the sidecar (sidecar.Meta.Report) instead, so
// whatever records a report (§15.12 ③) writes both, or the Planner and the
// page see two conclusions.
func (h HealthSources) indexDigest(e *health.IndexEntry) {
	if h.Digests == nil {
		return
	}
	e.Digest = h.Digests.Path(e.Key)
	if st, ok := h.Digests.State(e.Key); ok && st.Report != nil {
		e.Report = &health.IndexReport{State: st.Report.State, Summary: digest.FirstSentence(st.Report.Summary), At: st.Report.At}
	}
}

func sessionFact(snap session.SessionSnapshot, s *session.ManagedSession, logDir string) health.Fact {
	f := health.Fact{
		ID:       snap.Key,
		Kind:     health.KindSession,
		Source:   "router",
		Coverage: health.CoverageFull,
		Dir:      snap.Workspace,
		// A daemon's and the Planner's sessions run in naozhi's own state
		// directories, which are not a body of work.
		Register:   !sessionkey.IsSysKey(snap.Key) && !sessionkey.IsPlannerNSKey(snap.Key),
		LastActive: unixMilli(snap.LastActive),
		// At the millisecond, as LastActive: custody names a failure by it
		// and compares it with the ID in the sidecar, which may be one
		// LastActive gave.
		TurnAt: unixMilli(snap.TurnAt),
	}
	if b := snap.Backend; b != "" && b != "claude" {
		f.Coverage = health.CoverageGateway
	}
	setProcState(&f, snap.State)
	reason := snap.DeathReason
	if reason == "" && snap.State == "dead" && s != nil {
		reason = s.ProcessDeathReason()
	}
	switch sf := snap.StartupFailure; {
	case failedDeath(reason):
		f.Failed, f.ErrText = health.DeathEvidence+reason, snap.DeathDetail
	case sf != nil && sf.Streak > 0:
		f.Failed, f.ErrText = fmt.Sprintf("startup failed %d times (%s)", sf.Streak, sf.Class), snap.DeathDetail
		if sf.Class == "auth" {
			f.ErrClass = health.ErrAuth
		}
	case snap.State != "running" && apierr.IsEnvelope(snap.LastResponse):
		f.Failed, f.ErrText = "last turn ended in an API error", snap.LastResponse
	}
	if s != nil {
		bg := background(s, snap)
		f.Background = bg
		if t := watchedTask(bg, f.Running); t != nil {
			f.Workflow, f.WorkflowSeen = t.Summary, t.Last
			if t.Kind != bgtask.KindWorkflow {
				// Any event is an observation of the work: a turn waiting
				// on its task goes on writing.
				f.WorkflowSeen = later(t.Last, s.ProcessLastEventAt())
			}
		}
		// A dead process's tasks died with it, their ends unanswered.
		if snap.State != "dead" && !s.ProcessAttachedAt().IsZero() {
			f.Finished, f.Reply = finishedTask(s.EventLastN(finishScan))
		}
	}
	if !f.Running && f.Failed == "" && s != nil {
		f.Asking = askPending(s.EventLastN(askScan))
	}
	e := &health.IndexEntry{
		Key:        snap.Key,
		Title:      cmp.Or(snap.UserLabel, snap.Summary),
		LastActive: f.LastActive,
		Result:     snap.LastResponse,
		Background: health.NewIndexBackground(f.Background),
	}
	// kiro and codex accept no --name; what the session recorded was never
	// passed to their CLI.
	if s != nil && f.Coverage == health.CoverageFull {
		e.Name = s.CLISessionName()
	}
	if logDir != "" {
		e.EventLog = persist.LogPath(logDir, snap.Key)
	}
	f.Index = e
	return f
}

func cronFact(j cron.Job, running bool, now, started, since time.Time) health.Fact {
	f := health.Fact{
		ID:         sessionkey.CronKey(j.ID),
		Kind:       health.KindCron,
		Source:     "cron",
		Coverage:   health.CoverageFull,
		Dir:        j.WorkDir,
		Register:   true,
		Running:    running,
		LastActive: j.LastRunAt,
	}
	switch {
	case j.Paused && cron.IsAutoPausedReason(j.PausedReason):
		f.Failed, f.ErrText = "auto-paused ("+j.PausedReason+")", j.LastError
	case j.Paused:
	case j.LastError != "" && cronRunFailed(j.LastErrorClass):
		f.Failed, f.ErrText = "last run failed ("+cmp.Or(string(j.LastErrorClass), "error")+")", j.LastError
	case running:
		// LastRunAt moves only when a run ends: a long run, or the catch-up
		// after a wake, is not a miss.
	default:
		// Lateness counts from the last run, the job's creation or since,
		// whichever is latest: ticks slept through are not the job's.
		floor := since
		if j.CreatedAt.After(floor) {
			floor = j.CreatedAt
		}
		if j.LastRunAt.Before(floor) {
			j.LastRunAt = floor
		}
		if missed, at := cron.HasMissedSchedule(&j, now, started); missed {
			f.Missed = at
		}
	}
	return f
}

// cronRunFailed reports whether a run that ended with class failed the job,
// rather than being skipped or cut short for reasons outside it.
func cronRunFailed(class cron.ErrorClass) bool {
	switch class {
	case cron.ErrClassOverlapSkipped, cron.ErrClassSessionCapacity, cron.ErrClassBudgetExceeded,
		cron.ErrClassPausedConcurrent, cron.ErrClassDeletedConcurrent, cron.ErrClassCanceled,
		cron.ErrClassInterrupted, cron.ErrClassConfigDrift:
		return false
	}
	return true
}

func daemonFact(d sysession.DaemonStatus) health.Fact {
	f := health.Fact{
		ID:       sessionkey.SysKeyPrefix + d.Name,
		Kind:     health.KindSysession,
		Source:   "sysession",
		Coverage: health.CoverageGateway,
	}
	if d.LastRun != nil {
		f.LastActive = cmp.Or(d.LastRun.EndedAt, d.LastRun.StartedAt)
	}
	if !d.Enabled {
		f.Failed = fmt.Sprintf("circuit breaker tripped after %d CLI failures", d.ConsecutiveCLIFailures)
		if d.LastRun != nil {
			f.ErrText = d.LastRun.ErrorMsg
		}
	}
	return f
}

// remoteFact reads one session row of a remote node's cached /api/sessions.
// Its workspace is a directory on that node's machine.
func remoteFact(nodeID string, down bool, m map[string]any) (health.Fact, bool) {
	key, _ := m["key"].(string)
	if key == "" {
		return health.Fact{}, false
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	f := health.Fact{
		ID:       nodeID + "/" + key,
		Kind:     health.KindRemote,
		Source:   "node:" + nodeID,
		Coverage: health.CoverageSnapshot,
		Dir:      str("workspace"),
		Node:     nodeID,
		NodeDown: down,
	}
	setProcState(&f, str("state"))
	if r := str("death_reason"); failedDeath(r) {
		f.Failed = health.DeathEvidence + r
	}
	f.LastActive = unixMilli(jsonInt(m["last_active"]))
	f.Index = &health.IndexEntry{
		Key:        key,
		Node:       nodeID,
		Title:      cmp.Or(str("user_label"), str("summary")),
		LastActive: f.LastActive,
		Result:     str("last_response"),
	}
	return f, true
}

func externalFact(d discovery.DiscoveredSession) health.Fact {
	f := health.Fact{
		ID:         "external:" + cmp.Or(d.SessionID, fmt.Sprint(d.PID)),
		Kind:       health.KindExternal,
		Source:     "discovery",
		Coverage:   health.CoverageSnapshot,
		Dir:        d.CWD,
		LastActive: unixMilli(d.LastActive),
	}
	setProcState(&f, d.State)
	return f
}

// setProcState reads a snapshot's process state: running is a turn in
// progress, ready and dead are not, anything else is no data.
func setProcState(f *health.Fact, state string) {
	switch state {
	case "running":
		f.Running = true
	case "ready", "dead":
	default:
		f.NoData = true
	}
}

// failedDeath reports whether a death reason is an abnormal end (a crash,
// a kill from outside, a watchdog) rather than naozhi reclaiming the process
// or the CLI exiting cleanly.
func failedDeath(reason string) bool {
	switch reason {
	case cliinfo.DeathReasonNoOutputTimeout, cliinfo.DeathReasonTotalTimeout, cliinfo.DeathReasonReadLoopPanic,
		cliinfo.DeathReasonShimEOF, cliinfo.DeathReasonShimReadErr,
		cliinfo.DeathReasonShimOversizeThenEOF, cliinfo.DeathReasonShimOversizeThenErr,
		"process_exited":
		return true
	}
	return strings.HasPrefix(reason, cliinfo.DeathReasonCLIExitedCodePrefix) ||
		strings.HasPrefix(reason, cliinfo.DeathReasonCLIExitedSignalPrefix)
}

// watchedTask is the open task in bg whose quiet the health table judges
// (health.StallAfter): the first one, while a turn runs and waits on it. A
// Bash or a Monitor writes nothing between its start and its end, so in an
// idle session its quiet is how a server or a watch runs: it is open
// (后台中) but not judged stalled. nil when none is watched.
func watchedTask(bg bgtask.State, running bool) *bgtask.Task {
	for i, t := range bg.Tasks {
		if running || t.Kind != bgtask.KindBash && t.Kind != bgtask.KindMonitor {
			return &bg.Tasks[i]
		}
	}
	return nil
}

// replyRunes caps the first line of the reply a background task's end got.
const replyRunes = 120

// finishedTask is the latest background task in entries that ended with a
// turn's result after its end, and the first line of that turn's reply;
// nil when none. A message from the user after the task started means they
// took it over: they already have the reply.
func finishedTask(entries []clievent.EventEntry) (*health.Task, string) {
	names := map[string]string{}
	started := map[string]int{}
	lastUser := -1
	var ended, done *health.Task
	text, reply := "", ""
	for i, e := range entries {
		switch {
		case e.Type == clievent.KindTaskStart && e.TaskID != "":
			names[e.TaskID], started[e.TaskID] = e.Summary, i
		case e.Type == clievent.KindUser && e.Source != clievent.SourceCustody:
			lastUser, ended, done, reply = i, nil, nil, ""
		case bgtask.Ended(e):
			ended, text = nil, ""
			// A task whose start is out of the window ran during any
			// message before its end.
			if at, ok := started[e.TaskID]; lastUser < 0 || ok && lastUser < at {
				ended = &health.Task{ID: e.TaskID, Name: cmp.Or(names[e.TaskID], e.Summary, e.TaskID)}
			}
		case e.Type == clievent.KindText && ended != nil:
			text = cmp.Or(e.Detail, e.Summary)
		case e.Type == clievent.KindResult && ended != nil:
			done, reply, ended = ended, firstLine(text), nil
		}
	}
	return done, reply
}

// firstLine is s's first non-blank line, capped at replyRunes.
func firstLine(s string) string {
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			return textutil.TruncateRunes(line, replyRunes)
		}
	}
	return ""
}

// askPending reports whether the latest AskUserQuestion card has no user
// message after it.
func askPending(entries []clievent.EventEntry) bool {
	for i := len(entries) - 1; i >= 0; i-- {
		switch entries[i].Type {
		case clievent.KindUser:
			return false
		case clievent.KindAskQuestion:
			return true
		}
	}
	return false
}

func unixMilli(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// jsonInt reads a decoded JSON number.
func jsonInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}
