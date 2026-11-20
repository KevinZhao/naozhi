package wireup

import (
	"errors"
	"strings"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/textutil"
)

// PlannerRoutines is action.Routines over the cron scheduler: a routine
// block's standing task is a cron job like one made in 自动化, where the user
// sees, edits and removes it (docs/rfc/planner.md §15.17, D131).
type PlannerRoutines struct {
	Sched *cron.Scheduler
}

// errNoCron is a gateway with no cron scheduler.
var errNoCron = errors.New("这台网关没有开定时任务")

// Add implements action.Routines. Each run starts from a clean session:
// what a run should remember it keeps in a file of its workspace.
func (p PlannerRoutines) Add(title, dir, schedule, prompt string) error {
	if p.Sched == nil {
		return errNoCron
	}
	err := p.Sched.AddJob(&cron.Job{
		Schedule: schedule, Prompt: prompt, Title: title, WorkDir: dir, FreshContext: true,
		Platform: "dashboard", ChatID: "global", CreatedBy: "planner",
	})
	switch {
	case errors.Is(err, cron.ErrInvalidSchedule):
		return errors.New("schedule 不是有效的定时表达式（五段 cron，如 0 9 * * 1-5；或 @daily、@every 6h）")
	case errors.Is(err, cron.ErrPersistFailed):
		return errors.New("已加上，但没能存盘，重启后会丢")
	}
	return err
}

// Switch implements action.Routines.
func (p PlannerRoutines) Switch(id string, off bool) (string, error) {
	if p.Sched == nil {
		return "", errNoCron
	}
	var (
		job *cron.Job
		err error
	)
	if off {
		job, err = p.Sched.PauseJobByID(id)
	} else {
		job, err = p.Sched.ResumeJobByID(id)
	}
	switch {
	case errors.Is(err, cron.ErrJobNotFound):
		return "", errors.New("没有这个例行任务（id 照抄快照里方括号中的那串）")
	case errors.Is(err, cron.ErrJobAlreadyPaused), errors.Is(err, cron.ErrJobNotPaused):
		err = nil // already as asked
	case err != nil:
		return "", err
	}
	if job == nil {
		return "", nil
	}
	return routineTitle(*job), nil
}

// routineResultRunes caps a routine's last result in the snapshot.
const routineResultRunes = 60

// routineTitle is a job's title, else its prompt's first line.
func routineTitle(j cron.Job) string {
	if j.Title != "" {
		return j.Title
	}
	line, _, _ := strings.Cut(strings.TrimSpace(j.Prompt), "\n")
	return textutil.TruncateRunes(line, 40)
}

// PlannerRoutineList feeds the Planner's snapshot the standing tasks: every
// cron job, the user's own included. nil without a scheduler.
func PlannerRoutineList(sched *cron.Scheduler) func() []agent.Routine {
	if sched == nil {
		return nil
	}
	return func() []agent.Routine {
		jobs := sched.ListAllJobsWithNextRun()
		out := make([]agent.Routine, 0, len(jobs))
		for _, jw := range jobs {
			j := jw.Job
			line, _, _ := strings.Cut(strings.TrimSpace(j.LastResult), "\n")
			out = append(out, agent.Routine{ID: j.ID, Title: routineTitle(j), Schedule: j.Schedule, Paused: j.Paused,
				Failed: j.LastError != "", Result: textutil.TruncateRunes(line, routineResultRunes)})
		}
		return out
	}
}

// PlannerFronts feeds the Planner's snapshot the lines of work in progress.
func PlannerFronts(s *front.Store) func() []front.Front {
	if s == nil {
		return nil
	}
	return s.Open
}
