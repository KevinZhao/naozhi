package cron

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/naozhi/naozhi/internal/budget"
)

// ErrBudgetSpent is returned by ReplaySandboxRun when the job's or the
// machine's daily budget is spent; the dashboard maps it to 409.
var ErrBudgetSpent = errors.New("cron: daily budget spent")

// BudgetGate is the daily spend check a run passes before it spawns;
// satisfied by *budget.Gate. nil in SchedulerDeps runs every job.
type BudgetGate interface {
	CheckJob(id string) budget.Verdict
	Once(n budget.Notice, s budget.Subject) bool
}

// budgetSkipped reports whether today's spend refuses rc's run. A refused
// run ends skipped/budget_exceeded, which the auto-pause streak ignores. The
// job's first refusal each day is recorded and told to its chat; later ones
// only emit run_ended, so a frequent job cannot fill its history with skips.
// Under action warn the run goes ahead; the chat is told once a day when the
// job or the machine passes warn_ratio, and once more when it passes the cap.
func (s *Scheduler) budgetSkipped(rc runCtx) bool {
	if s.budget == nil {
		return false
	}
	v := s.budget.CheckJob(rc.jobID)
	if v.Blocked {
		rc.lg.Warn("cron run skipped: daily budget spent",
			"subject", string(v.Subject), "spent_usd", v.Spent, "limit_usd", v.Limit)
		first := s.budget.Once(budget.NoticeBlocked, budget.JobSubject(rc.jobID))
		s.finishRun(rc, runOutcome{
			state: RunStateSkipped, errClass: ErrClassBudgetExceeded,
			errMsg:      "daily budget spent (" + v.Subject.Kind() + " " + v.Usage() + ")",
			skipPersist: !first,
		})
		if first {
			s.deliverNotice(rc.notifyTo, formatCronNotice(rc.snap.labelOrID(), budgetBlockedNotice(v, rc.runID)))
		}
		return true
	}
	n := budget.NoticeWarn
	if v.Over {
		n = budget.NoticeOver
	}
	if v.Warn && s.budget.Once(n, v.Subject) {
		s.deliverNotice(rc.notifyTo, formatCronNotice(rc.snap.labelOrID(), budgetWarnNotice(v)))
	}
	return false
}

// replayBudgetErr wraps ErrBudgetSpent when today's spend refuses a replay
// of job id. A replay is a new run and passes the gate a TriggerNow does, but
// is refused before admission: no run is recorded and the attention entry
// stays for a retry after the reset.
func (s *Scheduler) replayBudgetErr(id string) error {
	if s.budget == nil {
		return nil
	}
	v := s.budget.CheckJob(id)
	if !v.Blocked {
		return nil
	}
	return fmt.Errorf("%w (%s %s, resets %s)", ErrBudgetSpent,
		v.Subject.Kind(), v.Usage(), v.ResetAt.Format("2006-01-02 15:04"))
}

// budgetScope names the subject a cron verdict is about.
func budgetScope(s budget.Subject) string {
	if s == budget.Global {
		return "本机"
	}
	return "本任务"
}

// budgetBlockedNotice is the body for a run the budget refused.
func budgetBlockedNotice(v budget.Verdict, runID string) string {
	body := "今日费用预算已用尽（" + budgetScope(v.Subject) + " " + v.Usage() + "），本次已跳过；" +
		v.ResetAt.Format("01-02 15:04") + " 重置"
	if len(runID) > 8 {
		runID = runID[:8]
	}
	if runID == "" {
		return body
	}
	return body + " · run " + runID
}

// budgetWarnNotice is the body for a run that goes ahead near or past a cap.
func budgetWarnNotice(v budget.Verdict) string {
	if v.Over {
		return "今日费用已超出预算（" + budgetScope(v.Subject) + " " + v.Usage() + "），本次照常执行"
	}
	pct := strconv.Itoa(int(v.Spent / v.Limit * 100))
	return "今日费用已达预算的 " + pct + "%（" + budgetScope(v.Subject) + " " + v.Usage() + "）"
}
