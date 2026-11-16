package action

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/bgtask"
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

// TestReportAlsoSentByPeer (D114): a progress report, the session's work
// still running, whose turn also sent the Planner a cross-session message
// starts no receipt turn: its line rides on the next receipt, and the
// conversation still gets its 📨 note. Done, blocked and need_user, and
// progress from a session stopped, say what only the gateway knows at the
// turn's end, and the message may have been a question asked on the way
// (the dispatch prompt has them so ask): they are told at once, saying the
// message came too, and done / blocked left 进行中 is reminded as ever. A
// message from an earlier turn is not this report.
func TestReportAlsoSentByPeer(t *testing.T) {
	for _, c := range []struct {
		name  string
		state string
		bg    bool
		old   bool
		along bool
	}{
		{"progress with work running", "progress", true, false, true},
		{"progress stopped", "progress", false, false, false},
		{"done", "done", false, false, false},
		{"blocked", "blocked", false, false, false},
		{"need_user", "need_user", false, false, false},
		{"old prompt done", "done", false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := endedRunner(t)
			var along []string
			e.r.cfg.TellAlong = func(text string) { along = append(along, text) }
			if c.bg {
				e.r.cfg.Background = func(string) bgtask.State {
					return bgtask.State{Tasks: []bgtask.Task{{ID: "m1", Kind: bgtask.KindMonitor, Last: e.now}}}.At(e.now)
				}
			}
			if c.old {
				e.sendMessagePrompt()
			}
			e.wait(time.Minute)
			e.peerAt(e.now)
			// The Planner answers the message, a question, in a turn of its own.
			e.planner("收到它的消息。")
			e.g.told = nil
			e.wait(time.Second)
			e.reportEnd(reportReply(c.state, "测试全部通过"))
			told := slices.ContainsFunc(e.g.told, func(x string) bool { return strings.Contains(x, "汇报：") })
			if c.along {
				if told || len(along) != 1 || !strings.Contains(along[0], e.key+"（iso 跑测试）汇报：") || !strings.Contains(along[0], "SendMessage 发给过你") {
					t.Fatalf("told = %q, along = %q, want the line along only", e.g.told, along)
				}
			} else {
				if !told || len(along) != 0 || !strings.Contains(e.g.told[len(e.g.told)-1], "也用 SendMessage 给你发过消息") {
					t.Fatalf("told = %q, along = %q, want the line told at once", e.g.told, along)
				}
			}
			if c.state == "progress" && !c.bg && !strings.Contains(e.g.told[len(e.g.told)-1], "不会自己接着做") {
				t.Errorf("told = %q, want the stopped hint", e.g.told)
			}
			if !slices.ContainsFunc(e.g.notes, func(n string) bool { return strings.HasPrefix(n, "📨 "+e.key+" 汇报：") }) {
				t.Errorf("notes = %q, want the 📨 line", e.g.notes)
			}
			e.g.told = nil
			e.wait(time.Minute)
			e.planner("好。")
			reminded := slices.ContainsFunc(e.g.told, func(x string) bool { return strings.Contains(x, "刚向你汇报") })
			if want := c.state == "done" || c.state == "blocked"; reminded != want {
				t.Errorf("told = %q, want the status reminder: %v", e.g.told, want)
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
		if len(along) != 0 || len(e.g.told) != 1 || !strings.Contains(e.g.told[0], "汇报：完成") || strings.Contains(e.g.told[0], "SendMessage") {
			t.Errorf("told = %q, along = %q, want the line told", e.g.told, along)
		}
	})
}

// TestStoppedProgressWhileChatting (D114): progress from a session stopped
// asks the Planner to act on what holds at the turn's end, so while the
// user is chatting with the Planner its check waits, the turn end left
// unchecked on disk, and runs afresh once they are quiet: still stopped, the
// line says so; with a turn of the session's own since, it does not.
func TestStoppedProgressWhileChatting(t *testing.T) {
	for _, later := range []bool{false, true} {
		e := endedRunner(t)
		quiet := e.now.Add(5 * time.Minute)
		e.r.cfg.ChatQuiet = func() time.Time { return quiet }
		e.reportEnd(reportReply("progress", "跑了一半"))
		if len(e.g.told) != 0 || len(e.held) != 1 {
			t.Fatalf("while chatting: told %q, held %d, want the check waiting", e.g.told, len(e.held))
		}
		if m, _ := e.side.Get(e.key); m.Turn == nil || !m.Turn.Check {
			t.Fatalf("turn = %+v, want it unchecked on disk", m.Turn)
		}
		if later {
			// The user has the session go another way; that turn's own
			// receipt is told as ever.
			e.wait(time.Minute)
			e.end("换了个做法，在跑")
		}
		e.wait(5 * time.Minute)
		e.fire()
		i := slices.IndexFunc(e.g.told, func(x string) bool { return strings.Contains(x, "汇报：进度") })
		if i < 0 {
			t.Fatalf("later %v: told %q, want the line once quiet", later, e.g.told)
		}
		if got := strings.Contains(e.g.told[i], "不会自己接着做"); got == later {
			t.Errorf("later %v: line = %q, want the stopped hint: %v", later, e.g.told[i], !later)
		}
	}
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
