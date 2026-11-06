package brief

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/custody"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/task"
)

func (f *fixture) custody(ts time.Time, unit, event string, reason custody.Reason) {
	f.t.Helper()
	data, _ := json.Marshal(note.CustodyData{Event: event, Reason: string(reason)})
	if err := f.j.Append(journal.Entry{TS: ts, Kind: journal.KindCustody, Unit: unit, Data: data}); err != nil {
		f.t.Fatal(err)
	}
}

// What the brief says of a unit: finished only by a turn's end the journal
// recorded, failed with what custody did, recovered as finished; the cron
// jobs whose result changed; a Midway credential running out before 09:00.
func TestBriefLines(t *testing.T) {
	const s = "dashboard:direct:a:general"
	snap := func(state health.State) health.Snapshot {
		return health.Snapshot{At: at(3, 9, 0), Units: []health.Unit{{ID: s, Kind: health.KindSession, Workspace: "ws1", State: state, LastActive: at(3, 1, 0)}}}
	}
	tests := []struct {
		name    string
		setup   func(f *fixture)
		want    []string
		notWant []string
	}{
		{"a restart dates no turn", func(f *fixture) {
			// Only the snapshot's last activity, as a restart leaves it.
			f.snap = snap(health.Idle)
		}, nil, []string{"完成的"}},
		{"a dispatched task stopped or failed did not finish", func(f *fixture) {
			for _, id := range []string{"task:a", "task:b"} {
				f.health(at(2, 20, 0), id, "ws1", health.Active, health.Idle, health.KindSession, "")
			}
			f.tasks = []task.Task{
				{ID: "task:a", Kind: task.KindSession, Title: "停了的", State: task.Done, By: task.ByUser, Why: task.StoppedWhy, Retired: at(2, 21, 0)},
				{ID: "task:b", Kind: task.KindSession, Title: "挂了的", State: task.Failed, Retired: at(2, 21, 0)},
			}
		}, nil, []string{"完成的"}},
		{"recovered after custody's retries", func(f *fixture) {
			f.health(at(2, 23, 0), s, "ws1", health.Active, health.Failed, health.KindSession, health.ErrNetwork)
			f.custody(at(2, 23, 1), s, "sent", custody.ReasonRetry)
			f.custody(at(2, 23, 3), s, "sent", custody.ReasonRetry)
			f.health(at(2, 23, 4), s, "ws1", health.Failed, health.Active, health.KindSession, "")
			f.health(at(2, 23, 6), s, "ws1", health.Active, health.Idle, health.KindSession, "")
			f.snap = snap(health.Idle)
		}, []string{"### 完成的（1）\n- [" + s + "](#session=dashboard%3Adirect%3Aa%3Ageneral) · 工作区 ws1 · 11/2 23:06 · 托管重试 2 次后恢复\n",
			"\n托管：重试 2 次（恢复 1、停下 0）\n"}, []string{"失败的"}},
		{"recovered by itself, not finished since", func(f *fixture) {
			f.health(at(2, 23, 0), s, "ws1", health.Active, health.Failed, health.KindSession, health.ErrNetwork)
			f.health(at(2, 23, 6), s, "ws1", health.Failed, health.Idle, health.KindSession, "")
			f.snap = snap(health.Idle)
		}, nil, []string{"失败的", "完成的", "托管："}},
		{"custody stopped", func(f *fixture) {
			f.health(at(2, 23, 0), s, "ws1", health.Active, health.Failed, health.KindSession, health.ErrNetwork)
			f.custody(at(2, 23, 1), s, "sent", custody.ReasonRetry)
			f.custody(at(2, 23, 9), s, "stopped", "")
			f.snap = snap(health.Failed)
		}, []string{"· 11/2 23:00 · 网络 / DNS · 托管已停\n", "\n托管：重试 1 次（恢复 0、停下 1）\n"}, nil},
		{"rate limited", func(f *fixture) {
			f.health(at(2, 23, 0), s, "ws1", health.Active, health.Failed, health.KindSession, health.ErrRateLimit)
			f.snap = snap(health.Failed)
		}, []string{"· 11/2 23:00 · 限流，托管不重试\n"}, []string{"托管："}},
		{"answered for the user", func(f *fixture) {
			f.custody(at(2, 23, 1), s, "sent", custody.ReasonSuggested)
		}, []string{"\n托管：代答 1 次\n"}, nil},
		{"routine results", func(f *fixture) {
			f.routines = []Routine{
				{Unit: "cron:bill", At: at(3, 8, 20), Changed: true, FirstLine: "本月 *$130*"},
				{Unit: "cron:sps", At: at(3, 8, 0), Changed: false},
			}
		}, []string{"### 例行任务有变化（1）\n- 账单 · 11/3 08:20 · 本月 ＊＄130＊\n- 另有 1 个例行任务结果没变\n"}, nil},
		{"routine results unchanged", func(f *fixture) {
			f.routines = []Routine{{Unit: "cron:sps", At: at(3, 8, 0)}}
		}, []string{"\n例行任务 1 个，结果都没变\n"}, []string{"例行任务有变化"}},
		{"Midway runs out this morning", func(f *fixture) {
			f.midway = at(3, 8, 50)
		}, []string{"\nMidway 凭证 08:50 到期，先 mwinit 再开始\n"}, nil},
		{"Midway already out", func(f *fixture) {
			f.midway = at(2, 22, 0)
		}, []string{"\nMidway 凭证已于 11/2 22:00 过期，先 mwinit 再开始\n"}, nil},
		{"Midway good until the afternoon", func(f *fixture) {
			f.midway = at(3, 15, 0)
		}, nil, []string{"Midway"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			// Something to report in every case, so the brief is sent.
			f.inbox = []notify.Item{{Kind: notify.KindError, State: notify.StateOpen, Source: "x", Title: "另一个错误", Created: at(3, 8, 0)}}
			tt.setup(f)
			f.now = at(3, 8, 30)
			b := f.briefer(map[string]string{"cron:bill": "账单", "cron:sps": "容量"}, nil)
			b.Check()
			got := b.Recent(1)
			if len(got) != 1 {
				t.Fatal("no brief sent")
			}
			md := got[0].Markdown
			for _, w := range tt.want {
				if !strings.Contains(md, w) {
					t.Errorf("markdown lacks %q:\n%s", w, md)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(md, w) {
					t.Errorf("markdown has %q:\n%s", w, md)
				}
			}
		})
	}
}
