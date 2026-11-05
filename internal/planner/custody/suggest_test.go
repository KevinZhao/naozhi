package custody

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
)

func TestRecommended(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  bool
	}{
		{"先跑测试 (Recommended)", true},
		{"先跑测试（推荐）", true},
		{"Run tests (Recommended)  ", true},
		{"先跑测试", false},
		{"(Recommended) 先跑测试", false},
		{"先跑测试 (recommended)", false},
		{"先跑测试 (推荐)", false},
		{"推荐：先跑测试", false},
	} {
		if got := Recommended(tc.label); got != tc.want {
			t.Errorf("Recommended(%q) = %v, want %v", tc.label, got, tc.want)
		}
	}
}

func item(header, question string, labels ...string) clievent.AskQuestionItem {
	q := clievent.AskQuestionItem{Header: header, Question: question}
	for _, l := range labels {
		q.Options = append(q.Options, clievent.AskQuestionOpt{Label: l})
	}
	return q
}

func TestSuggest(t *testing.T) {
	multi := item("改哪些", "要改哪些文件？", "a.go (Recommended)", "b.go", "c.go（推荐）")
	multi.MultiSelect = true
	described := item("方案", "怎么做？", "方案一 (Recommended)", "方案二")
	described.Options[0].Description = "做完直接 push 到远端"
	otherIrreversible := item("方案", "怎么做？", "先跑测试 (Recommended)", "直接 push")
	for _, tc := range []struct {
		name   string
		items  []clievent.AskQuestionItem
		act    Act
		answer string
	}{
		{"one recommended", []clievent.AskQuestionItem{item("方案", "先做哪个？", "先跑测试 (Recommended)", "先改文档")}, ActAnswer, "方案: 先跑测试 (Recommended)."},
		{"chinese mark, no header", []clievent.AskQuestionItem{item("", "先做哪个？", "先改文档", "先跑测试（推荐）")}, ActAnswer, "先跑测试（推荐）."},
		{"every question recommended", []clievent.AskQuestionItem{item("A", "q1", "x (Recommended)", "y"), item("B", "q2", "z", "w（推荐）")}, ActAnswer, "A: x (Recommended). B: w（推荐）."},
		{"multi-select takes every recommended", []clievent.AskQuestionItem{multi}, ActAnswer, "改哪些: a.go (Recommended), c.go（推荐）."},
		{"an option not recommended may be irreversible", []clievent.AskQuestionItem{otherIrreversible}, ActAnswer, "方案: 先跑测试 (Recommended)."},
		{"none recommended", []clievent.AskQuestionItem{item("方案", "先做哪个？", "先跑测试", "先改文档")}, ActHandOff, ""},
		{"no question recommended", []clievent.AskQuestionItem{item("A", "q1", "x", "y"), item("B", "q2", "z", "w")}, ActHandOff, ""},
		{"some questions not recommended", []clievent.AskQuestionItem{item("A", "q1", "x (Recommended)", "y"), item("B", "q2", "z", "w")}, ActUser, ""},
		{"two recommended in a single choice", []clievent.AskQuestionItem{item("A", "q1", "x (Recommended)", "y (Recommended)")}, ActUser, ""},
		{"irreversible question", []clievent.AskQuestionItem{item("发布", "现在 push 吗？", "好 (Recommended)", "先不")}, ActUser, ""},
		{"irreversible recommended description", []clievent.AskQuestionItem{described}, ActUser, ""},
		{"irreversible in one question of many", []clievent.AskQuestionItem{item("A", "q1", "x (Recommended)"), item("B", "q2", "合并到 master (Recommended)")}, ActUser, ""},
		{"irreversible question without a recommendation", []clievent.AskQuestionItem{item("", "要部署吗？", "要", "不要")}, ActUser, ""},
		{"no questions", nil, ActUser, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Suggest(clievent.AskQuestion{ToolUseID: "t1", Items: tc.items})
			if s.Act != tc.act || s.Answer != tc.answer {
				t.Errorf("Suggest = %+v, want act %v answer %q", s, tc.act, tc.answer)
			}
			if s.Act == ActUser && s.Why == "" {
				t.Error("left to the user without a why")
			}
		})
	}
}

// Every keyword of the gateway's list (D93) in the recommended option keeps
// custody from answering it.
func TestSuggestIrreversibleKeywords(t *testing.T) {
	for _, opt := range []string{
		"现在 push", "git push origin main", "Merge the PR", "合并到 master", "Cut a release", "现在发版", "打 tag v1.2.0", "git tag v1.2.0",
		"给客户发邮件", "在群里发消息", "对外发送报告", "删除远程仓库", "删除分支 feat/x", "删除资源组", "deploy to prod", "部署到测试环境",
		"去付款", "用信用卡支付",
	} {
		for _, mark := range recommendedMarks {
			card := clievent.AskQuestion{Items: []clievent.AskQuestionItem{item("下一步", "接下来做什么？", opt+" "+mark, "先停下")}}
			if s := Suggest(card); s.Act != ActUser {
				t.Errorf("Suggest(%q) = %+v, want it left to the user", opt+" "+mark, s)
			}
		}
	}
}

// askRig is a rig whose sessions hold AskUserQuestion cards.
type askRig struct {
	*rig
	events  map[string][]clievent.EventEntry
	handOff []string
	handErr error
	user    []string
}

func newAskRig(t *testing.T) *askRig {
	a := &askRig{rig: newRig(t), events: map[string][]clievent.EventEntry{}}
	a.e.cfg.Events = func(key string, n int) []clievent.EventEntry {
		ev := a.events[key]
		return ev[max(0, len(ev)-n):]
	}
	a.e.cfg.HandOff = func(key, what string) error {
		a.handOff = append(a.handOff, key+"|"+what)
		return a.handErr
	}
	a.e.cfg.SendUser = func(key, text string) error { a.user = append(a.user, key+"|"+text); return nil }
	return a
}

// ask has key put up card and wait on the user.
func (a *askRig) ask(key, id string, items ...clievent.AskQuestionItem) {
	a.events[key] = append(a.events[key], clievent.EventEntry{Time: a.now.UnixMilli(), Type: clievent.KindAskQuestion,
		AskQuestion: &clievent.AskQuestion{ToolUseID: id, Items: items}})
	a.units = []health.Unit{{ID: key, Kind: health.KindSession, State: health.Waiting, Workspace: "ws1"}}
}

// tick advances the clock by d and ticks.
func (a *askRig) tick(d time.Duration) {
	a.now = a.now.Add(d)
	a.e.Tick()
}

// A card with a recommended option is answered once it waited SuggestAfter,
// through Deliver (prefixed, counted), the way the dashboard composes a
// click; once only.
func TestAnswerAsksRecommended(t *testing.T) {
	a := newAskRig(t)
	a.dispatch("task:a")
	a.ask("task:a", "t1", item("方案", "先做哪个？", "先跑测试 (Recommended)", "先改文档"))
	a.tick(0)
	a.tick(SuggestAfter - time.Second)
	if len(a.sent) != 0 {
		t.Fatalf("answered before %s: %v", SuggestAfter, a.sent)
	}
	a.tick(time.Second)
	want := "task:a|" + Prefix(ReasonSuggested, 1) + "\n方案: 先跑测试 (Recommended)."
	if len(a.sent) != 1 || a.sent[0] != want {
		t.Fatalf("sent = %q, want %q", a.sent, want)
	}
	a.tick(time.Minute)
	if len(a.sent) != 1 {
		t.Errorf("answered twice: %q", a.sent)
	}
	if got := a.custodyEvents("task:a"); len(got) == 0 || !strings.Contains(got[len(got)-1], `"sent"`) || !strings.Contains(got[len(got)-1], string(ReasonSuggested)) {
		t.Errorf("notes = %v, want a sent one", got)
	}
}

// The window counts only while the user is away: at the session it stops
// (noted once as held back) and resumes after; the user's own answer
// cancels it, and a new card starts afresh.
func TestAnswerAsksWindow(t *testing.T) {
	a := newAskRig(t)
	a.dispatch("task:a")
	a.ask("task:a", "t1", item("方案", "先做哪个？", "先跑测试 (Recommended)", "先改文档"))
	a.tick(0)
	a.tick(time.Minute)
	a.lastUser["task:a"] = a.now // nearby for NearbyFor
	for range 4 {
		a.tick(time.Minute)
	}
	if len(a.sent) != 0 {
		t.Fatalf("answered with the user nearby: %v", a.sent)
	}
	if got := a.custodyEvents("task:a"); len(got) != 1 || !strings.Contains(got[0], `"deferred"`) {
		t.Errorf("notes = %v, want one deferred", got)
	}
	a.tick(time.Minute) // the user left: counting again from here
	a.tick(time.Minute - time.Second)
	if len(a.sent) != 0 {
		t.Fatalf("the time the user was nearby counted: %v", a.sent)
	}
	a.tick(time.Second)
	if len(a.sent) != 1 {
		t.Fatalf("not answered after %s away: %v", SuggestAfter, a.sent)
	}

	// The user answers the next card themselves: custody leaves it.
	a.sent = nil
	a.ask("task:a", "t2", item("方案", "再做哪个？", "补测试 (Recommended)", "停下"))
	a.tick(0)
	a.tick(time.Minute)
	a.events["task:a"] = append(a.events["task:a"], clievent.EventEntry{Type: clievent.KindUser, Detail: "停下."})
	a.tick(time.Minute) // health has not seen the answer yet
	a.ask("task:a", "t3", item("方案", "然后？", "收尾 (Recommended)", "停下"))
	a.tick(0)
	a.tick(SuggestAfter - time.Second)
	if len(a.sent) != 0 {
		t.Fatalf("answered a card the user answered, or a new card early: %v", a.sent)
	}
	a.tick(time.Second)
	if len(a.sent) != 1 || !strings.Contains(a.sent[0], "收尾") {
		t.Errorf("sent = %v, want the new card answered", a.sent)
	}
}

// Nothing is answered for a session out of custody, nor while custody is
// paused, which stops the window too.
func TestAnswerAsksOffAndPaused(t *testing.T) {
	a := newAskRig(t)
	a.ask(dash, "t1", item("方案", "先做哪个？", "先跑测试 (Recommended)"))
	a.tick(0)
	a.tick(SuggestAfter)
	if len(a.sent) != 0 || len(a.inbox.items) != 0 {
		t.Fatalf("custody acted out of custody: sent=%v inbox=%v", a.sent, a.inbox.items)
	}

	a.dispatch("task:a")
	a.ask("task:a", "t1", item("方案", "先做哪个？", "先跑测试 (Recommended)"))
	a.tick(0)
	if err := a.e.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	a.tick(time.Minute)
	a.tick(10 * time.Minute)
	if len(a.sent) != 0 {
		t.Fatalf("answered while paused: %v", a.sent)
	}
	if err := a.e.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	a.tick(0)
	a.tick(SuggestAfter - time.Second)
	if len(a.sent) != 0 {
		t.Fatalf("the pause counted: %v", a.sent)
	}
	a.tick(time.Second)
	if len(a.sent) != 1 {
		t.Errorf("not answered after the pause: %v", a.sent)
	}
}

// An irreversible card, or one only partly recommended, goes to the user's
// inbox as the session waiting on them, at once; one with no recommended
// option goes to the Planner, unless the Planner may not answer it either.
func TestAnswerAsksLeftAndHandedOff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		items   []clievent.AskQuestionItem
		handErr error
		noPlan  bool
		handed  bool
		inbox   bool
	}{
		{name: "irreversible", items: []clievent.AskQuestionItem{item("下一步", "接下来？", "push 到远端 (Recommended)", "先停下")}, inbox: true},
		{name: "partly recommended", items: []clievent.AskQuestionItem{item("A", "q1", "x (Recommended)"), item("B", "q2", "y", "z")}, inbox: true},
		{name: "no recommendation", items: []clievent.AskQuestionItem{item("方案", "先做哪个？", "先跑测试", "先改文档")}, handed: true},
		{name: "no recommendation, irreversible option", items: []clievent.AskQuestionItem{item("方案", "先做哪个？", "先跑测试", "直接合并")}, handErr: ErrIrreversible, handed: true, inbox: true},
		{name: "no recommendation, Planner stopped", items: []clievent.AskQuestionItem{item("方案", "先做哪个？", "先跑测试", "先改文档")}, handErr: errors.New("planner stopped"), handed: true, inbox: true},
		{name: "no recommendation, no Planner", items: []clievent.AskQuestionItem{item("方案", "先做哪个？", "先跑测试", "先改文档")}, noPlan: true, inbox: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAskRig(t)
			a.handErr = tc.handErr
			if tc.noPlan {
				a.e.cfg.HandOff = nil
			}
			a.dispatch("task:a")
			a.ask("task:a", "t1", tc.items...)
			a.tick(0)
			a.tick(SuggestAfter)
			a.tick(time.Minute)
			if len(a.sent) != 0 {
				t.Errorf("answered: %v", a.sent)
			}
			if got := len(a.handOff) == 1; got != tc.handed || len(a.handOff) > 1 {
				t.Errorf("handed off = %q, want %v once", a.handOff, tc.handed)
			}
			if tc.handed && !strings.Contains(a.handOff[0], "先做哪个？") || tc.handed && !strings.Contains(a.handOff[0], "先改文档") && !strings.Contains(a.handOff[0], "直接合并") {
				t.Errorf("hand-off lacks the question or its options: %q", a.handOff)
			}
			if !tc.inbox {
				if len(a.inbox.items) != 0 {
					t.Errorf("inbox = %+v, want none", a.inbox.items)
				}
				return
			}
			if len(a.inbox.items) != 1 {
				t.Fatalf("inbox = %+v, want one item", a.inbox.items)
			}
			it := a.inbox.items[0]
			if it.Kind != notify.KindQuestion || it.Source != "waiting:task:a" || it.Unit != "task:a" || !it.AutoClose || it.Why == "" || !strings.Contains(it.Title, "在等你回复") {
				t.Errorf("inbox item = %+v", it)
			}
		})
	}
}

// Undo sends the take-back of the latest recommended answer as the user's
// own message; a Planner's go-ahead or the user's message is not one.
func TestUndo(t *testing.T) {
	a := newAskRig(t)
	a.dispatch("task:a")
	if _, err := a.e.Undo("task:a"); !errors.Is(err, ErrNoSuggested) {
		t.Errorf("Undo with nothing answered = %v", err)
	}
	a.events["task:a"] = []clievent.EventEntry{
		{Type: clievent.KindUser, Source: clievent.SourceCustody, Detail: Prefix(ReasonSuggested, 1) + "\n方案: 先跑测试 (Recommended)."},
		{Type: clievent.KindUser, Source: clievent.SourceCustody, Detail: Prefix(ReasonPlanner, 2) + "\n继续"},
		{Type: clievent.KindUser, Detail: Prefix(ReasonSuggested, 3) + "\n用户自己贴的"},
	}
	got, err := a.e.Undo("task:a")
	if err != nil || got != "方案: 先跑测试 (Recommended)." {
		t.Fatalf("Undo = %q, %v", got, err)
	}
	want := "task:a|撤回刚才按建议选的 方案: 先跑测试 (Recommended)，先停下等我决定"
	if len(a.user) != 1 || a.user[0] != want || len(a.sent) != 0 {
		t.Errorf("sent as the user %q, as custody %q; want %q", a.user, a.sent, want)
	}
	if _, err := a.e.Undo("remote/x"); !errors.Is(err, ErrNoSession) {
		t.Errorf("Undo of a remote session = %v", err)
	}
}
