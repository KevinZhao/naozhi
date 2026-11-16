package action

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/task"
)

// peerAt has the rig's Reports answer a cross-session message from its
// session that reached the Planner at at, and none other.
func (e *endRig) peerAt(at time.Time) {
	e.r.cfg.Reports = func(from, to time.Time) []string {
		if at.After(from) && !at.After(to) {
			return []string{e.key}
		}
		return nil
	}
}

// TestReportAlsoSentByPeer (D114): a report block whose turn also sent the
// Planner a cross-session message starts no receipt turn: its line, with
// the reminder to mark the task its reply left 进行中, rides on the next
// receipt; the conversation still gets its 📨 note, and that reply is not
// reminded again. A message from an earlier turn is not this report.
func TestReportAlsoSentByPeer(t *testing.T) {
	for _, c := range []struct {
		name     string
		state    string
		old      bool
		reminded bool
	}{
		{"done", "done", false, true},
		{"blocked", "blocked", false, true},
		{"progress", "progress", false, false},
		{"old prompt", "done", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := endedRunner(t)
			var along []string
			e.r.cfg.TellAlong = func(text string) { along = append(along, text) }
			if c.old {
				e.sendMessagePrompt()
			}
			e.wait(time.Minute)
			e.peerAt(e.now)
			// The Planner answers the message in a turn of its own.
			e.planner("收到它的消息。")
			e.g.told = nil
			e.wait(time.Second)
			e.reportEnd(reportReply(c.state, "测试全部通过"))
			if slices.ContainsFunc(e.g.told, func(x string) bool { return strings.Contains(x, "汇报：") }) {
				t.Fatalf("told = %q, want no turn for the report", e.g.told)
			}
			if len(along) != 1 || !strings.Contains(along[0], e.key+"（iso 跑测试）汇报：") || !strings.Contains(along[0], "已经用 SendMessage 发给过你") {
				t.Fatalf("along = %q", along)
			}
			if got := strings.Contains(along[0], "仍是进行中"); got != c.reminded {
				t.Errorf("along = %q, want the status reminder: %v", along[0], c.reminded)
			}
			if !slices.ContainsFunc(e.g.notes, func(n string) bool { return strings.HasPrefix(n, "📨 "+e.key+" 汇报：") }) {
				t.Errorf("notes = %q, want the 📨 line", e.g.notes)
			}
			e.wait(time.Minute)
			e.planner("好。")
			if slices.ContainsFunc(e.g.told, func(x string) bool { return strings.Contains(x, "刚向你汇报") }) {
				t.Errorf("told = %q, want no reminder turn", e.g.told)
			}
		})
	}

	t.Run("earlier turn's message", func(t *testing.T) {
		e := endedRunner(t)
		var along []string
		e.r.cfg.TellAlong = func(text string) { along = append(along, text) }
		e.peerAt(e.now.Add(time.Second))
		e.wait(time.Minute)
		e.end("问过了，接着做")
		e.g.told = nil
		e.wait(time.Minute)
		e.reportEnd(reportReply("done", "做完了"))
		if len(along) != 0 || len(e.g.told) != 1 || !strings.Contains(e.g.told[0], "汇报：完成") {
			t.Errorf("told = %q, along = %q, want the line told", e.g.told, along)
		}
	})
}

// TestProgressClassLater (D114): a progress report's line and a dispatch's
// receipt go through TellLater, the one the agent holds while the user is
// chatting; done, blocked and need_user, and a failed dispatch, are told at
// once.
func TestProgressClassLater(t *testing.T) {
	for _, c := range []struct {
		state string
		later bool
	}{
		{"progress", true},
		{"done", false},
		{"blocked", false},
		{"need_user", false},
	} {
		t.Run(c.state, func(t *testing.T) {
			e := endedRunner(t)
			var later []string
			e.r.cfg.TellLater = func(text string) error { later = append(later, text); return nil }
			e.reportEnd(reportReply(c.state, "跑了一半"))
			told := slices.ContainsFunc(e.g.told, func(x string) bool { return strings.Contains(x, "汇报：") })
			if got := len(later) == 1 && strings.Contains(later[0], "汇报："); got != c.later || told == c.later {
				t.Errorf("later = %q, told = %q, want later: %v", later, e.g.told, c.later)
			}
		})
	}

	r, g, _, side, dir := newRunner(t)
	r.cfg.Tasks = task.New(task.Config{Sessions: side})
	var later []string
	r.cfg.TellLater = func(text string) error { later = append(later, text); return nil }
	r.Handle(block(`{"type":"dispatch","workspace":"` + filepath.Base(dir) + `","title":"iso 写 hello","model":"","message":"写一个 hello.md"}`))
	if len(later) != 1 || !strings.HasPrefix(later[0], "已派出：iso 写 hello") || len(g.told) != 0 {
		t.Errorf("dispatch: later = %q, told = %q", later, g.told)
	}
	g.spawnErr = errors.New("拉不起来")
	later, g.told = nil, nil
	r.Handle(block(`{"type":"dispatch","workspace":"` + filepath.Base(dir) + `","title":"iso 写 world","model":"","message":"写一个 world.md"}`))
	if len(later) != 0 || len(g.told) != 1 || !strings.Contains(g.told[0], "失败") {
		t.Errorf("failed dispatch: later = %q, told = %q", later, g.told)
	}
}
