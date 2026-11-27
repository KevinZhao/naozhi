package bgtask

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// The progress line says how long the work has run, each workflow's agents
// done of how many and its phase, and the other tasks by name; what is not
// known is left out, not made up.
func TestProgress(t *testing.T) {
	now := time.Date(2026, 11, 23, 15, 0, 0, 0, time.Local)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	long := strings.Repeat("长", 60)
	tests := []struct {
		name  string
		tasks []Task
		want  string
	}{
		{name: "none open"},
		{name: "a Bash", tasks: []Task{{ID: "b1", Kind: KindBash, Summary: "npm run bench", Start: ago(80 * time.Minute), Last: ago(80 * time.Minute)}},
			want: "已跑 1 小时 20 分钟 · 1 个后台任务：npm run bench"},
		{name: "under a minute", tasks: []Task{{ID: "b1", Kind: KindBash, Summary: "make", Start: ago(20 * time.Second)}},
			want: "刚开始 · 1 个后台任务：make"},
		{name: "a workflow with its counts and phase", tasks: []Task{{ID: "w1", Kind: KindWorkflow, Summary: "review-all", Start: ago(45 * time.Minute),
			Agents: 12, Done: 5, Phase: "验证"}},
			want: "已跑 45 分钟 · 工作流 review-all：12 个子代理完成 5 个，阶段「验证」"},
		{name: "failed agents are said", tasks: []Task{{ID: "w1", Kind: KindWorkflow, Summary: "review-all", Start: ago(time.Hour),
			Agents: 12, Done: 5, Failed: 2}},
			want: "已跑 1 小时 · 工作流 review-all：12 个子代理完成 5 个、失败 2 个"},
		{name: "a workflow the board says nothing of, its start not known", tasks: []Task{{ID: "w1", Kind: KindWorkflow, Summary: "w1"}},
			want: "工作流 w1"},
		{name: "only the phase known", tasks: []Task{{ID: "w1", Kind: KindWorkflow, Summary: "fix", Phase: "实现"}},
			want: "工作流 fix：阶段「实现」"},
		{name: "a workflow and tasks: the earliest start, the first two named", tasks: []Task{
			{ID: "w1", Kind: KindWorkflow, Summary: "review-all", Start: ago(10 * time.Minute), Agents: 3, Done: 3, Phase: "汇总"},
			{ID: "b1", Kind: KindBash, Summary: "tail -f\n  build.log", Start: ago(2 * time.Hour)},
			{ID: "m1", Kind: KindMonitor, Summary: "watch CI", Start: ago(time.Hour)},
			{ID: "a1", Kind: KindAgent, Summary: "explore", Start: ago(time.Hour)}},
			want: "已跑 2 小时 · 工作流 review-all：3 个子代理完成 3 个，阶段「汇总」 · 3 个后台任务：tail -f build.log、watch CI 等"},
		{name: "a start not known among those known: no duration", tasks: []Task{
			{ID: "r1", Kind: KindBash, Summary: "deploy", Start: ago(10 * time.Minute), Last: ago(10 * time.Minute), Replayed: true},
			{ID: "b1", Kind: KindBash, Summary: "bench", Start: ago(5 * time.Minute)},
			{ID: "w1", Kind: KindWorkflow, Summary: "fix"}},
			want: "工作流 fix · 2 个后台任务：deploy、bench"},
		{name: "more workflows than are named", tasks: []Task{
			{ID: "w1", Kind: KindWorkflow, Summary: "a"}, {ID: "w2", Kind: KindWorkflow, Summary: "b"}, {ID: "w3", Kind: KindWorkflow, Summary: "c"}},
			want: "工作流 a · 工作流 b · 另有 1 个工作流"},
		{name: "a start after now adds no duration", tasks: []Task{{ID: "b1", Kind: KindBash, Summary: "make", Start: now.Add(time.Minute)}},
			want: "1 个后台任务：make"},
		{name: "a long name is cut", tasks: []Task{{ID: "b1", Kind: KindBash, Summary: long}},
			want: "1 个后台任务：" + strings.Repeat("长", 40) + "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (State{Tasks: tt.tasks}).Progress(now); got != tt.want {
				t.Errorf("Progress = %q\nwant       %q", got, tt.want)
			}
		})
	}
}

// A task only a shim's replay started ran before the gateway attached, for
// how long is not known: its line says no duration, where one whose start
// is in the event log says its own.
func TestProgressReplayed(t *testing.T) {
	now := time.Date(2026, 11, 23, 15, 0, 0, 0, time.Local)
	in := Input{Alive: true, Since: now.Add(-4 * time.Hour), Attached: now.Add(-10 * time.Minute), Now: now,
		Replayed: []clievent.EventEntry{{Type: clievent.KindTaskStart, TaskID: "b1", TaskType: "local_bash", Summary: "npm run bench"}}}
	s := Scan(in).At(in.Now)
	if got := s.Progress(now); got != "1 个后台任务：npm run bench" {
		t.Errorf("Progress of a task the replay alone started = %q, want no duration", got)
	}
	if got := s.Output(); !got.IsZero() {
		t.Errorf("Output = %v, want none: the attach is no write of the task's", got)
	}
	in.Entries = []clievent.EventEntry{
		{Type: clievent.KindToolUse, Tool: "Bash", Time: now.Add(-3 * time.Hour).UnixMilli()},
		{Type: clievent.KindTaskStart, TaskID: "b1", TaskType: "local_bash", Summary: "npm run bench", Time: now.Add(-3 * time.Hour).UnixMilli()},
		{Type: clievent.KindResult, Time: now.Add(-3*time.Hour).UnixMilli() + 1},
	}
	if got := Scan(in).At(in.Now).Progress(now); got != "已跑 3 小时 · 1 个后台任务：npm run bench" {
		t.Errorf("Progress with the start in the log = %q", got)
	}
}

// Output is the latest write after a task's start: a Bash that only
// started has written nothing.
func TestOutput(t *testing.T) {
	at := time.Date(2026, 11, 23, 15, 0, 0, 0, time.Local)
	s := State{Tasks: []Task{
		{ID: "b1", Kind: KindBash, Start: at, Last: at},
		{ID: "m1", Kind: KindMonitor, Start: at.Add(time.Hour), Last: at.Add(time.Hour)},
	}}
	if got := s.Output(); !got.IsZero() {
		t.Errorf("Output of tasks that only started = %v, want none", got)
	}
	s.Tasks = append(s.Tasks,
		Task{ID: "a1", Kind: KindAgent, Start: at, Last: at.Add(10 * time.Minute)},
		Task{ID: "w1", Kind: KindWorkflow, Last: at.Add(5 * time.Minute)})
	if got := s.Output(); !got.Equal(at.Add(10 * time.Minute)) {
		t.Errorf("Output = %v, want the agent's progress", got)
	}
}

// Phases names the phase of each workflow that has one; WithWorkflows
// carries a board's counts and phase onto its task.
func TestPhases(t *testing.T) {
	at := time.Date(2026, 11, 23, 15, 0, 0, 0, time.Local)
	s := WithWorkflows(State{Tasks: []Task{{ID: "b1", Kind: KindBash, Phase: "not a workflow's"}}}, []Workflow{
		{ID: "w1", Name: "review", Start: at, Agents: 4, Done: 1, Failed: 1, Phase: "验证"},
		{ID: "w2", Name: "fix", Start: at},
	})
	if got := s.Phases(); !maps.Equal(got, map[string]string{"w1": "验证"}) {
		t.Errorf("Phases = %v, want w1 alone", got)
	}
	if w := s.Tasks[0]; w.Agents != 4 || w.Done != 1 || w.Failed != 1 || w.Phase != "验证" {
		t.Errorf("workflow task = %+v, want the board's counts and phase", w)
	}
}
