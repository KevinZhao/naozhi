package wireup

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
)

// A routine block's standing task is a cron job like one made in 自动化:
// fresh context, the workspace's directory, the Planner as its maker; the
// snapshot lists it by its ID, which pauses and resumes it. A schedule the
// scheduler refuses is said in words the Planner can act on.
func TestPlannerRoutines(t *testing.T) {
	sched := cron.NewScheduler(cron.SchedulerConfig{MaxJobs: 4, AllowNilRouter: true}, cron.SchedulerDeps{
		Agents: map[string]cron.AgentOpts{"general": {}},
	})
	p := PlannerRoutines{Sched: sched}
	dir := t.TempDir()
	if err := p.Add("内网模型动态", dir, "0 9 * * 1-5", "查内网 Anthropic 新模型动态\n第一行写结论"); err != nil {
		t.Fatal(err)
	}
	list := PlannerRoutineList(sched)()
	if len(list) != 1 || list[0].Title != "内网模型动态" || list[0].Schedule != "0 9 * * 1-5" || list[0].Paused || list[0].ID == "" {
		t.Fatalf("list = %+v", list)
	}
	job, ok := sched.GetJob(list[0].ID)
	if !ok || !job.FreshContext || job.WorkDir != dir || job.CreatedBy != "planner" || job.Platform != "dashboard" {
		t.Fatalf("job = %+v", job)
	}

	if title, err := p.Switch(list[0].ID, true); err != nil || title != "内网模型动态" {
		t.Fatalf("pause: %q, %v", title, err)
	}
	if _, err := p.Switch(list[0].ID, true); err != nil {
		t.Errorf("pausing a paused one: %v", err)
	}
	if got := PlannerRoutineList(sched)(); !got[0].Paused {
		t.Errorf("not paused: %+v", got)
	}
	if _, err := p.Switch(list[0].ID, false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := PlannerRoutineList(sched)(); got[0].Paused {
		t.Errorf("not resumed: %+v", got)
	}
	if _, err := p.Switch("nope", true); err == nil || !strings.Contains(err.Error(), "没有这个例行任务") {
		t.Errorf("unknown id: %v", err)
	}
	if err := p.Add("坏的", dir, "每天九点", "p"); err == nil || !strings.Contains(err.Error(), "schedule 不是有效的定时表达式") {
		t.Errorf("bad schedule: %v", err)
	}
	if err := (PlannerRoutines{}).Add("t", dir, "@daily", "p"); err == nil {
		t.Error("no scheduler: added")
	}
	if PlannerRoutineList(nil) != nil {
		t.Error("no scheduler: a list")
	}
}
