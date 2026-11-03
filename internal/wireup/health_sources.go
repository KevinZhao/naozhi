package wireup

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/cliinfo"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/sysession"
)

// askScan is how many trailing events are searched for an AskUserQuestion
// the user has not answered yet; a card further back is not counted.
const askScan = 64

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
	if h.Router != nil {
		for _, snap := range h.Router.ListSessions() {
			out = append(out, sessionFact(snap, h.Router.SessionFor(snap.Key), h.EventLogDir))
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
		f.Failed, f.ErrText = "death_reason="+reason, snap.DeathDetail
	case sf != nil && sf.Streak > 0:
		f.Failed, f.ErrText = fmt.Sprintf("startup failed %d times (%s)", sf.Streak, sf.Class), snap.DeathDetail
		if sf.Class == "auth" {
			f.ErrClass = health.ErrAuth
		}
	case snap.State != "running" && apierr.IsEnvelope(snap.LastResponse):
		f.Failed, f.ErrText = "last turn ended in an API error", snap.LastResponse
	}
	if name := runningWorkflow(snap.Workflows); name != "" && s != nil {
		f.Workflow, f.WorkflowSeen = name, unixMilli(s.WorkflowBoard().LastObservedAt())
	}
	if !f.Running && f.Failed == "" && s != nil {
		f.Asking = askPending(s.EventLastN(askScan))
	}
	e := &health.IndexEntry{
		Key:        snap.Key,
		Title:      cmp.Or(snap.UserLabel, snap.Summary),
		LastActive: f.LastActive,
		Result:     snap.LastResponse,
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
func remoteFact(nodeID string, down bool, m map[string]any) (health.Fact, bool) {
	key, _ := m["key"].(string)
	if key == "" {
		return health.Fact{}, false
	}
	f := health.Fact{
		ID:       nodeID + "/" + key,
		Kind:     health.KindRemote,
		Source:   "node:" + nodeID,
		Coverage: health.CoverageSnapshot,
		Node:     nodeID,
		NodeDown: down,
	}
	state, _ := m["state"].(string)
	setProcState(&f, state)
	if r, _ := m["death_reason"].(string); failedDeath(r) {
		f.Failed = "death_reason=" + r
	}
	f.LastActive = unixMilli(jsonInt(m["last_active"]))
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

// runningWorkflow names the first running workflow; a paused one waits on
// purpose and is not watched for stalls.
func runningWorkflow(sums []workflow.Summary) string {
	for _, s := range sums {
		if s.Status == workflow.StatusRunning {
			return cmp.Or(s.Name, s.TaskID)
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
