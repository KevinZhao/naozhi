package action

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/brief"
	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
)

// fakePatrol is the project patrol's findings as the runner sees them: the
// refs on record, the answers it gives a turn with none of the user's
// words, and the calls made.
type fakePatrol struct {
	// ws is the workspace ID the findings are in; known the refs on record.
	ws    string
	known map[string]bool
	// deny answers CanDispatch and capped Surface, in an unasked turn.
	deny, capped error
	// fileErr fails File of a known finding; inHand has File give it no
	// item (it is in a task's hands, or over).
	fileErr error
	inHand  bool
	// card is the Decision a notice about a known finding gets; left and
	// decideErr what Decided answers.
	card      *notify.Decision
	left      string
	decideErr error
	calls     []string
}

var errNoFinding = errors.New("没有这条巡查发现")

func (p *fakePatrol) CanDispatch(ref string, unasked bool) (string, error) {
	p.calls = append(p.calls, fmt.Sprintf("can %s unasked=%v", ref, unasked))
	switch {
	case !p.known[ref]:
		return "", errNoFinding
	case unasked && p.deny != nil:
		return "", p.deny
	}
	return p.ws, nil
}

func (p *fakePatrol) Dispatched(ref, task string) {
	p.calls = append(p.calls, "dispatched "+ref+" "+task)
}

func (p *fakePatrol) Reported(ref, task, state string, prs []string) {
	p.calls = append(p.calls, "reported "+ref+" "+task+" "+state+" "+strings.Join(prs, ","))
}

func (p *fakePatrol) Surface(ref, kind string, unasked bool) error {
	p.calls = append(p.calls, fmt.Sprintf("surface %s %s unasked=%v", ref, kind, unasked))
	switch {
	case !p.known[ref]:
		return errNoFinding
	case unasked && p.capped != nil:
		return p.capped
	}
	return nil
}

func (p *fakePatrol) File(ref, text, line string) (bool, bool, error) {
	p.calls = append(p.calls, "file "+ref+" "+text+" "+line)
	if !p.known[ref] {
		return false, false, nil
	}
	return true, p.fileErr == nil && !p.inHand, p.fileErr
}

func (p *fakePatrol) Card(ref string) *notify.Decision {
	p.calls = append(p.calls, "card "+ref)
	if !p.known[ref] {
		return nil
	}
	return p.card
}

func (p *fakePatrol) Decided(ref, choice string) (string, error) {
	p.calls = append(p.calls, "decided "+ref+" "+choice)
	return p.left, p.decideErr
}

// did is the calls made that start with prefix.
func (p *fakePatrol) did(prefix string) []string {
	var out []string
	for _, c := range p.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// patrolTurn is a runner in a turn a deep check's findings started (or any
// other, once patrol is set false): its receipts of blocks not run go to
// refusals.
type patrolTurn struct {
	t        *testing.T
	r        *Runner
	g        *fakeGateway
	inbox    *notify.Notifier
	side     *sidecar.Store
	p        *fakePatrol
	ws       string // the registered workspace's name
	patrol   bool
	unrun    bool
	refusals []string
	// held counts the notices kept for the morning brief.
	held int
}

const (
	refA = "ws_ab12cd34/bug-router-nil"
	refB = "ws_ab12cd34/debt-dup-backoff"
)

func newPatrolTurn(t *testing.T) *patrolTurn {
	t.Helper()
	r, g, inbox, side, dir := newRunner(t)
	w, _, ok := r.cfg.Workspaces.Find(filepath.Base(dir))
	if !ok {
		t.Fatal("workspace not found")
	}
	pt := &patrolTurn{t: t, r: r, g: g, inbox: inbox, side: side, ws: w.Name, patrol: true,
		p: &fakePatrol{ws: w.ID, known: map[string]bool{refA: true, refB: true}}}
	userTurn(r) // no words: the turn is not the user's
	r.cfg.Patrol = pt.p
	r.cfg.PatrolTurn = func() bool { return pt.patrol }
	r.cfg.UnrunTurn = func() bool { return pt.unrun }
	r.cfg.TellPatrolRefused = func(text string) error { pt.refusals = append(pt.refusals, text); return nil }
	r.cfg.Promises = promise.New(promise.Config{})
	r.cfg.Tasks = task.New(task.Config{Sessions: side, Promises: r.cfg.Promises})
	r.cfg.SideNote = func(string, string, string) error { return nil }
	r.cfg.Brief = func(brief.Mention) error { pt.held++; return nil }
	return pt
}

func (pt *patrolTurn) run(text string) {
	pt.r.Result(pt.r.cfg.Planner, text)
	pt.r.handle(<-pt.r.ch)
}

func (pt *patrolTurn) dispatch(extra string) string {
	return block(`{"type":"dispatch","workspace":"` + pt.ws + `","title":"修重连","model":"","message":"修掉重连时的空指针，开 PR"` + extra + `}`)
}

// A turn a deep check's findings started runs only dispatch, front, notify,
// ask and note (§15.23 ③), and of those a dispatch, a question and a timely
// notice only when they name their finding, so the caps count them: the
// rest is refused by the turn's source, and the receipt goes out as a turn
// held to the same limits.
func TestPatrolTurnRefuses(t *testing.T) {
	pt := newPatrolTurn(t)
	fronts := fronter(pt.r, nil)
	if err := pt.side.Put(sidecar.Meta{Key: "task:ab12", Purpose: sidecar.PurposeTask, Title: "iso 写 hello"}); err != nil {
		t.Fatal(err)
	}
	pt.run(strings.Join([]string{
		block(`{"type":"status","task":"task:ab12","state":"done","why":"看起来做完了"}`),
		block(`{"type":"promise","text":"周五前修完","due":"2026-11-27T18:00:00+08:00","close_when":"PR 合并"}`),
		block(`{"type":"custody","key":"task:ab12","text":"继续"}`),
		pt.dispatch(``), // names no finding
		block(`{"type":"front","id":"gw","name":"网关"}`),
		block(`{"type":"notify","title":"巡查看了一遍","why":"没有要紧的","level":"brief","link":"","id":""}`),
		block(`{"type":"ask","question":"要不要统一退避？","options":["要","先不"]}`),                         // names no finding
		block(`{"type":"notify","title":"有个要紧的事","why":"现在看","level":"timely","link":"","id":""}`), // timely, names no finding
		block(`{"type":"ask","question":"退避要不要统一？","options":["要","先不"],"finding":"` + refB + `"}`),
		block(`{"type":"note","key":"` + dashKey + `","text":"这里有个相关的发现","why":"巡查"}`),
	}, "\n"))
	if len(pt.g.spawned) != 0 {
		t.Errorf("spawned %q in a patrol turn with no finding named", pt.g.spawned)
	}
	if m, _ := pt.side.Get("task:ab12"); m.Mark != nil {
		t.Errorf("status ran: %+v", m.Mark)
	}
	if open, _ := pt.r.cfg.Promises.List(0); len(open) != 0 {
		t.Errorf("promise ran: %+v", open)
	}
	if len(*fronts) != 1 {
		t.Errorf("front blocks run: %d, want 1", len(*fronts))
	}
	if open, _ := pt.inbox.List(0); len(open) != 1 || open[0].Kind != notify.KindQuestion || open[0].Title != "退避要不要统一？" {
		t.Errorf("inbox %+v, want only the question that names its finding", open)
	}
	if pt.held != 1 {
		t.Errorf("%d notices held for the brief, want the one that names no finding", pt.held)
	}
	if len(pt.refusals) != 1 || len(pt.g.unrun)+len(pt.g.refused)+len(pt.g.told) != 0 {
		t.Fatalf("receipts: patrol %q unrun %q refused %q told %q; want one through TellPatrolRefused", pt.refusals, pt.g.unrun, pt.g.refused, pt.g.told)
	}
	receipt := pt.refusals[0]
	for _, want := range []string{
		"有 6 个动作块没有执行", "第 1 块（status「task:ab12」）", "第 2 块（promise", "第 3 块（custody",
		"第 4 块（dispatch「修重连」）：这一轮由项目巡查的发现触发，dispatch 要带 finding",
		"第 7 块（ask「要不要统一退避？」）：这一轮由项目巡查的发现触发，ask 要带 finding",
		"第 8 块（notify「有个要紧的事」）：这一轮由项目巡查的发现触发，timely 的 notify 要带 finding",
		"只能用 dispatch（带 finding）、front、notify、ask、note，没有执行",
		"回这条回执的这一轮也一样，别的块重发也不执行",
	} {
		if !strings.Contains(receipt, want) {
			t.Errorf("receipt lacks %q:\n%s", want, receipt)
		}
	}
	// Refused by the turn's source: no block of it is said to be malformed.
	for _, n := range pt.g.notes {
		if UnrunNote(n) {
			t.Errorf("a refusal by source reads as a block written wrong: %q", n)
		}
	}
	if len(pt.g.notes) != 6 || !strings.Contains(pt.g.notes[3], "派活要带 finding：「修重连」没有派出") {
		t.Errorf("conversation lines %q", pt.g.notes)
	}

	// The turn answering that receipt is held to the same and gets none of
	// its own: the two do not go round.
	pt.refusals, pt.unrun = nil, true
	pt.run(block(`{"type":"status","task":"task:ab12","state":"done","why":"再说一次"}`) + "\n" + pt.dispatch(``))
	if m, _ := pt.side.Get("task:ab12"); m.Mark != nil || len(pt.g.spawned) != 0 {
		t.Errorf("the receipt's turn ran what the patrol turn was refused: mark %+v spawned %q", m.Mark, pt.g.spawned)
	}
	if len(pt.refusals)+len(pt.g.unrun)+len(pt.g.told) != 0 {
		t.Errorf("the receipt's turn got a receipt: %q %q %q", pt.refusals, pt.g.unrun, pt.g.told)
	}
}

// A dispatch naming a finding the patrol lets go starts the task: its
// sidecar keeps the finding, the session is told what such a task ends at,
// and the patrol records the task.
func TestPatrolDispatch(t *testing.T) {
	pt := newPatrolTurn(t)
	pt.run(pt.dispatch(`,"finding":"` + refA + `"`))
	if len(pt.g.spawned) != 1 {
		t.Fatalf("spawned %q, notes %q, receipts %q", pt.g.spawned, pt.g.notes, pt.refusals)
	}
	key := pt.g.spawned[0]
	s := pt.g.spawns[0]
	if s.Finding != refA || !strings.HasPrefix(s.Message, "修掉重连时的空指针，开 PR\n\n（网关附注：这是项目巡查发现的问题，没有用户的原话。") ||
		!strings.Contains(s.Message, "不要合并、不要推到默认分支、不要打 tag 或发版、不要删除远端的东西。）") {
		t.Errorf("spawn %+v", s)
	}
	// The dashboard strips a gateway note up to the first full-width
	// closing parenthesis: the note holds one, its last rune.
	if strings.Count(patrolNote, "）") != 1 || !strings.HasSuffix(patrolNote, "）") {
		t.Errorf("patrolNote %q", patrolNote)
	}
	if m, ok := pt.side.Get(key); !ok || m.Finding != refA {
		t.Errorf("sidecar %+v", m)
	}
	if want := []string{"can " + refA + " unasked=true", "dispatched " + refA + " " + key}; !slices.Equal(pt.p.calls, want) {
		t.Errorf("patrol calls %q, want %q", pt.p.calls, want)
	}
	if len(pt.refusals)+len(pt.g.notes) != 0 || len(pt.g.told) != 1 || !strings.Contains(pt.g.told[0], "已派出：修重连") {
		t.Errorf("receipts %q notes %q told %q", pt.refusals, pt.g.notes, pt.g.told)
	}

	// A receipt's turn, not a patrol turn: a dispatch naming a finding is
	// held to the same conditions, the turn having none of the user's words.
	pt = newPatrolTurn(t)
	pt.patrol = false
	pt.run(pt.dispatch(`,"finding":"` + refA + `"`))
	if len(pt.g.spawns) != 1 || !strings.Contains(pt.g.spawns[0].Message, "网关附注") || pt.p.calls[0] != "can "+refA+" unasked=true" {
		t.Errorf("a receipt's turn: spawns %+v, calls %q", pt.g.spawns, pt.p.calls)
	}

	// The user asking: no conditions, and the message is theirs to word.
	pt = newPatrolTurn(t)
	pt.patrol = false
	userTurn(pt.r, said)
	pt.p.deny = errors.New("这条发现不是 S")
	pt.run(pt.dispatch(`,"finding":"` + refA + `"`))
	if len(pt.g.spawns) != 1 || pt.g.spawns[0].Message != "修掉重连时的空指针，开 PR" || pt.g.spawns[0].Finding != refA || pt.p.calls[0] != "can "+refA+" unasked=false" {
		t.Errorf("the user's turn: spawns %+v, calls %q", pt.g.spawns, pt.p.calls)
	}
}

// A dispatch the patrol refuses (a cap, a finding it may not have fixed on
// its own) starts nothing: the gateway files the finding as an open item
// and says so, so the Planner does not send it again.
func TestPatrolDispatchRefused(t *testing.T) {
	pt := newPatrolTurn(t)
	pt.p.deny = errors.New("这个工作区巡查派出的任务已有 2 个在做（上限 2）")
	pt.run(pt.dispatch(`,"finding":"` + refA + `"`))
	if len(pt.g.spawned) != 0 {
		t.Fatalf("spawned %q past the cap", pt.g.spawned)
	}
	if left := pt.side.List(); len(left) != 0 {
		t.Errorf("sidecars left: %+v", left)
	}
	if want := []string{"can " + refA + " unasked=true", "file " + refA + "  "}; !slices.Equal(pt.p.calls, want) {
		t.Errorf("patrol calls %q, want %q", pt.p.calls, want)
	}
	if len(pt.refusals) != 1 || !strings.Contains(pt.refusals[0], "第 1 块（dispatch「修重连」）：这个工作区巡查派出的任务已有 2 个在做（上限 2）；网关已把这条发现记进未了事项，不用重发") {
		t.Errorf("receipt %q", pt.refusals)
	}
	if strings.Contains(pt.refusals[0], "格式不对") || len(pt.g.told) != 0 {
		t.Errorf("receipt %q told %q: a cap is no malformed block and gets one receipt", pt.refusals, pt.g.told)
	}
	if len(pt.g.notes) != 1 || pt.g.notes[0] != "「修重连」没有派出：这个工作区巡查派出的任务已有 2 个在做（上限 2）；这条发现已记进未了事项" || UnrunNote(pt.g.notes[0]) {
		t.Errorf("conversation lines %q", pt.g.notes)
	}

	// Filing it failed too: said.
	pt = newPatrolTurn(t)
	pt.p.deny, pt.p.fileErr = errors.New("这条发现不是 S"), front.ErrItemsFull
	pt.run(pt.dispatch(`,"finding":"` + refA + `"`))
	if len(pt.refusals) != 1 || !strings.Contains(pt.refusals[0], "这条发现不是 S；网关也没能把这条发现记进未了事项（"+front.ErrItemsFull.Error()+"）") {
		t.Errorf("receipt %q", pt.refusals)
	}

	// Already in a task's hands: nothing to file, and the receipt does not
	// say it was.
	pt = newPatrolTurn(t)
	pt.p.deny, pt.p.inHand = errors.New("这条发现已经派给 task:t1 在做"), true
	pt.run(pt.dispatch(`,"finding":"` + refA + `"`))
	if len(pt.g.spawned) != 0 || len(pt.refusals) != 1 || !strings.Contains(pt.refusals[0], "这条发现已经派给 task:t1 在做；不用重发") ||
		strings.Contains(pt.refusals[0], "记进未了事项") || len(pt.g.notes) != 1 || strings.Contains(pt.g.notes[0], "未了事项") {
		t.Errorf("in a task's hands: spawned %q, receipt %q, notes %q", pt.g.spawned, pt.refusals, pt.g.notes)
	}

	// A ref no finding has is the block's fault, shown like a malformed one.
	pt = newPatrolTurn(t)
	pt.run(pt.dispatch(`,"finding":"ws_ab12cd34/bug-nope"`))
	if len(pt.g.spawned) != 0 || len(pt.refusals) != 1 || !strings.Contains(pt.refusals[0], errNoFinding.Error()) || !strings.Contains(pt.refusals[0], "格式不对的块") {
		t.Errorf("unknown finding: spawned %q, receipt %q", pt.g.spawned, pt.refusals)
	}

	// The finding is another workspace's than the dispatch names.
	pt = newPatrolTurn(t)
	pt.p.ws = "ws_zzzzzzz9"
	pt.run(pt.dispatch(`,"finding":"` + refA + `"`))
	if len(pt.g.spawned) != 0 || len(pt.refusals) != 1 || !strings.Contains(pt.refusals[0], "不是 workspace「"+pt.ws+"」的发现") {
		t.Errorf("another workspace's finding: spawned %q, receipt %q", pt.g.spawned, pt.refusals)
	}

	// No patrol on this gateway: the block is not run as a plain dispatch.
	pt = newPatrolTurn(t)
	pt.patrol, pt.r.cfg.Patrol = false, nil
	pt.run(pt.dispatch(`,"finding":"` + refA + `"`))
	if len(pt.g.spawned) != 0 || len(pt.g.unrun) != 1 || !strings.Contains(pt.g.unrun[0], "没有开项目巡查") {
		t.Errorf("no patrol: spawned %q, receipt %q", pt.g.spawned, pt.g.unrun)
	}
}

// A patrol turn has none of the user's words: a dispatch asking for them,
// for write tools or for ultracode is refused as in any such turn, finding
// or not.
func TestPatrolDispatchNeedsNoUserWords(t *testing.T) {
	for _, extra := range []string{`,"with_user_words":true`, `,"mcp_tools":{"aws-outlook-mcp":["email_send"]}`, `,"ultracode":true`} {
		pt := newPatrolTurn(t)
		pt.run(pt.dispatch(`,"finding":"` + refA + `"` + extra))
		if len(pt.g.spawned) != 0 || len(pt.p.calls) != 0 {
			t.Errorf("%s: spawned %q, patrol calls %q", extra, pt.g.spawned, pt.p.calls)
		}
		if len(pt.refusals) != 1 || !strings.Contains(pt.refusals[0], "这一轮不是用户发起的，不能带用户原话、mcp_tools 或 ultracode 派活") {
			t.Errorf("%s: receipt %q", extra, pt.refusals)
		}
	}
}

// A question or a timely notice about a finding counts against the day's
// cap of findings put in front of the user: past it nothing reaches them,
// the finding is filed, and the receipt says so. A notice for the brief is
// not counted.
func TestPatrolSurfaceCap(t *testing.T) {
	const ask = `{"type":"ask","question":"退避要不要统一成一个实现？","options":["统一","先不动"],"finding":"` + refB + `"}`
	const ready = `{"type":"notify","title":"修重连的 PR 已备好","why":"CI 通过","level":"timely","link":"https://github.com/owner/repo/pull/31","id":"","finding":"` + refA + `"}`
	pt := newPatrolTurn(t)
	pt.run(block(ask) + "\n" + block(ready))
	open, _ := pt.inbox.List(0)
	if len(open) != 2 || len(pt.refusals)+len(pt.g.notes) != 0 {
		t.Fatalf("inbox %+v, receipts %q, notes %q", open, pt.refusals, pt.g.notes)
	}
	if want := []string{"surface " + refB + " ask unasked=true", "surface " + refA + " pr unasked=true", "card " + refA}; !slices.Equal(pt.p.calls, want) {
		t.Errorf("patrol calls %q, want %q", pt.p.calls, want)
	}
	card := openDecision(t, pt.inbox, HandlerAsk)
	if !strings.Contains(string(card.Decision.Payload), `"finding":"`+refB+`"`) {
		t.Errorf("ask card payload %s lacks the finding", card.Decision.Payload)
	}

	pt = newPatrolTurn(t)
	pt.p.capped = errors.New("今天提到用户面前的巡查事项已有 3 条")
	pt.run(block(ask) + "\n" + block(ready))
	if open, _ := pt.inbox.List(0); len(open) != 0 {
		t.Fatalf("past the cap the inbox has %+v", open)
	}
	if got := pt.p.did("file "); !slices.Equal(got, []string{"file " + refB + "  ", "file " + refA + "  "}) {
		t.Errorf("filed %q", got)
	}
	if len(pt.refusals) != 1 || strings.Count(pt.refusals[0], "今天提到用户面前的巡查事项已有 3 条；网关已把这条发现记进未了事项，不用重发") != 2 ||
		strings.Contains(pt.refusals[0], "格式不对") {
		t.Errorf("receipt %q", pt.refusals)
	}
	if len(pt.g.notes) != 2 || !strings.Contains(pt.g.notes[1], "「修重连的 PR 已备好」没有推送") || UnrunNote(pt.g.notes[0]) {
		t.Errorf("conversation lines %q", pt.g.notes)
	}

	// A notice held for the brief is not put in front of the user now.
	pt = newPatrolTurn(t)
	pt.p.capped = errors.New("今天提到用户面前的巡查事项已有 3 条")
	pt.run(block(strings.Replace(ready, `"timely"`, `"brief"`, 1)))
	if pt.held != 1 || len(pt.p.calls) != 0 || len(pt.refusals) != 0 {
		t.Errorf("a brief notice: held %d, patrol calls %q, receipts %q", pt.held, pt.p.calls, pt.refusals)
	}

	// The user's own turn is not counted: the patrol is told so.
	pt = newPatrolTurn(t)
	pt.patrol = false
	userTurn(pt.r, said)
	pt.p.capped = errors.New("今天提到用户面前的巡查事项已有 3 条")
	pt.run(block(ask))
	if open, _ := pt.inbox.List(0); len(open) != 1 || pt.p.calls[0] != "surface "+refB+" ask unasked=false" {
		t.Errorf("the user's turn: inbox %+v, calls %q", open, pt.p.calls)
	}

	// A finding not on record: the block's fault, nothing asked.
	pt = newPatrolTurn(t)
	pt.run(block(strings.Replace(ask, refB, "ws_ab12cd34/bug-nope", 1)))
	if open, _ := pt.inbox.List(0); len(open) != 0 || len(pt.refusals) != 1 || !strings.Contains(pt.refusals[0], errNoFinding.Error()) {
		t.Errorf("unknown finding: inbox %+v, receipt %q", open, pt.refusals)
	}
}

// The user's say on a finding rides on what puts it in front of them
// (§15.23 ④): a notice about it carries the patrol's card, whose answers go
// to the patrol's own handler; a question about it gets, after the
// Planner's options, the two that turn it down.
func TestPatrolChoices(t *testing.T) {
	const ready = `{"type":"notify","title":"修重连的 PR 已备好","why":"CI 通过","level":"timely","link":"https://github.com/owner/repo/pull/31","id":"","finding":"` + refA + `"}`
	pt := newPatrolTurn(t)
	pt.p.card = &notify.Decision{Handler: "patrol", Payload: []byte(`{"ref":"` + refA + `"}`), Actions: []notify.Action{"不做", "这类别再提"}, More: []notify.Action{"这个工作区别巡查了"}}
	pt.run(block(ready))
	card := openDecision(t, pt.inbox, "patrol")
	if card.Kind != notify.KindNotice || card.Title != "修重连的 PR 已备好" || card.Link != "https://github.com/owner/repo/pull/31" ||
		!slices.Equal(card.Decision.Actions, pt.p.card.Actions) || !slices.Equal(card.Decision.More, pt.p.card.More) || string(card.Decision.Payload) != `{"ref":"`+refA+`"}` {
		t.Errorf("the notice %+v, decision %+v", card, card.Decision)
	}
	// A notice about nothing of the patrol's carries none; nor does one
	// the patrol has no card for.
	pt = newPatrolTurn(t)
	pt.patrol = false
	userTurn(pt.r, said)
	pt.p.card = &notify.Decision{Handler: "patrol", Actions: []notify.Action{"不做"}}
	pt.run(block(`{"type":"notify","title":"有个要紧的事","why":"现在看","level":"timely","link":"","id":""}`))
	if open, _ := pt.inbox.List(0); len(open) != 1 || open[0].Decision != nil || len(pt.p.did("card ")) != 0 {
		t.Errorf("a notice naming no finding: %+v, patrol calls %q", open, pt.p.calls)
	}
	pt = newPatrolTurn(t)
	pt.run(block(ready))
	if open, _ := pt.inbox.List(0); len(open) != 1 || open[0].Decision != nil {
		t.Errorf("a notice the patrol has no card for: %+v", open)
	}

	for name, tc := range map[string]struct {
		options string
		want    []notify.Action
		why     string
	}{
		"the Planner's options":   {`["统一","先不动"]`, []notify.Action{"统一", "先不动", "不做", "这类别再提"}, "Planner 问你"},
		"one of them the Planner": {`["统一","不做"]`, []notify.Action{"统一", "不做", "这类别再提"}, "Planner 问你"},
		"none of the Planner's":   {`[]`, []notify.Action{"不做", "这类别再提"}, "Planner 问你；要做就在 Planner 对话里回复，回复后关掉这一条"},
	} {
		pt = newPatrolTurn(t)
		pt.run(block(`{"type":"ask","question":"退避要不要统一？","options":` + tc.options + `,"finding":"` + refB + `"}`))
		card := openDecision(t, pt.inbox, HandlerAsk)
		if !slices.Equal(card.Decision.Actions, tc.want) || card.Why != tc.why || AskFinding(card) != refB {
			t.Errorf("%s: answers %q, why %q, finding %q", name, card.Decision.Actions, card.Why, AskFinding(card))
		}
	}
	// A question about no finding is as the Planner wrote it.
	pt = newPatrolTurn(t)
	pt.patrol = false
	userTurn(pt.r, said)
	pt.run(block(`{"type":"ask","question":"要不要统一退避？","options":["要","先不"]}`))
	if card := openDecision(t, pt.inbox, HandlerAsk); !slices.Equal(card.Decision.Actions, []notify.Action{"要", "先不"}) || AskFinding(card) != "" {
		t.Errorf("a question naming no finding: %+v", card.Decision)
	}
}

// The answer to a question about a finding goes to the patrol first: the
// two answers the gateway added turn the finding down, any other counts it
// as taken; the Planner gets its receipt either way, with what the patrol
// says is left to do. An answer the patrol could not record is not passed
// on and leaves the question open.
func TestPatrolAnswer(t *testing.T) {
	const ask = `{"type":"ask","question":"退避要不要统一？","options":["统一","先不动"],"finding":"` + refB + `"}`
	for name, tc := range map[string]struct {
		answer notify.Action
		choice string
		left   string
	}{
		"turned down":          {"不做", "skip", "网关已把这条发现记进被否清单并关掉了它的未了事项，不用再处理它。"},
		"its kind stopped":     {"这类别再提", "mute_kind", "网关已把这条发现记进被否清单并关掉了它的未了事项，不用再处理它。任务 task:t1 还在修它：用 cancel 把这个任务停掉。"},
		"one of the Planner's": {"统一", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			pt := newPatrolTurn(t)
			var answers []Words
			var told []string
			pt.r.cfg.TellAnswer = func(text string, w Words) error {
				told, answers = append(told, text), append(answers, w)
				return nil
			}
			pt.run(block(ask))
			card := openDecision(t, pt.inbox, HandlerAsk)
			pt.p.left = tc.left
			if err := pt.inbox.Decide(card.ID, tc.answer); err != nil {
				t.Fatal(err)
			}
			if got := pt.p.did("decided "); !slices.Equal(got, []string{"decided " + refB + " " + tc.choice}) {
				t.Errorf("patrol calls %q", got)
			}
			want := "你问用户的「退避要不要统一？」，用户的回答：" + string(tc.answer)
			if tc.left != "" {
				want += "。" + tc.left
			}
			if len(told) != 1 || told[0] != want || answers[0].Text != string(tc.answer) || answers[0].Question != "退避要不要统一？" {
				t.Errorf("the Planner was told %q with %+v, want %q", told, answers, want)
			}
			if got, _ := pt.inbox.Item(card.ID); got.State != notify.StateClosed {
				t.Errorf("the question is %s after its answer", got.State)
			}
		})
	}

	// The patrol could not write it down: nothing goes on, the question
	// stays.
	pt := newPatrolTurn(t)
	told := 0
	pt.r.cfg.TellAnswer = func(string, Words) error { told++; return nil }
	pt.run(block(ask))
	card := openDecision(t, pt.inbox, HandlerAsk)
	pt.p.decideErr = errors.New("被否清单 declined.json 读不了")
	if err := pt.inbox.Decide(card.ID, "不做"); err == nil || !strings.Contains(err.Error(), "declined.json 读不了") {
		t.Errorf("Decide = %v, want the patrol's error", err)
	}
	if got, _ := pt.inbox.Item(card.ID); got.State != notify.StateOpen || told != 0 {
		t.Errorf("the question is %s, the Planner told %d times", got.State, told)
	}
	// Turned down with the Planner out of reach: the finding is turned
	// down all the same, so the question closes.
	pt.p.decideErr, pt.p.left = nil, "网关已把这条发现记进被否清单并关掉了它的未了事项，不用再处理它。"
	pt.r.cfg.TellAnswer = func(string, Words) error { return errors.New("planner is stopped") }
	if err := pt.inbox.Decide(card.ID, "不做"); err != nil {
		t.Errorf("turned down with the Planner stopped: %v", err)
	}
	// One of the Planner's own with it out of reach stays open, as any
	// answer does.
	pt = newPatrolTurn(t)
	pt.r.cfg.TellAnswer = func(string, Words) error { return errors.New("planner is stopped") }
	pt.run(block(ask))
	card = openDecision(t, pt.inbox, HandlerAsk)
	if err := pt.inbox.Decide(card.ID, "统一"); err == nil {
		t.Error("an answer that reached nobody closed the question")
	}

	// A question about no finding does not come to the patrol.
	pt = newPatrolTurn(t)
	pt.patrol = false
	userTurn(pt.r, said)
	pt.r.cfg.TellAnswer = func(string, Words) error { return nil }
	pt.run(block(`{"type":"ask","question":"要不要统一退避？","options":["要","不做"]}`))
	if err := pt.inbox.Decide(openDecision(t, pt.inbox, HandlerAsk).ID, "不做"); err != nil || len(pt.p.did("decided ")) != 0 {
		t.Errorf("a question naming no finding: %v, patrol calls %q", err, pt.p.calls)
	}
}

// Two notices about one finding are one thing, whatever their titles: it is
// pushed once a day.
func TestPatrolNotifyKey(t *testing.T) {
	a := Notify{Title: "修重连的 PR 已备好", Finding: refA}
	b := Notify{Title: "PR #31 可以看了", Link: "https://github.com/owner/repo/pull/31", Finding: refA}
	if notifyKey(a) != notifyKey(b) {
		t.Error("two notices about one finding have two keys")
	}
	if notifyKey(a) == notifyKey(Notify{Title: a.Title, Finding: refB}) || notifyKey(a) == notifyKey(Notify{Title: a.Title}) {
		t.Error("another finding, or none, shares the key")
	}
	// A scout finding's ID still names the thing first.
	if notifyKey(Notify{ID: "m1", Finding: refA}) != notifyKey(Notify{ID: "m1"}) {
		t.Error("an ID no longer names the thing")
	}
}

// A front block's item whose source is a finding is filed by the patrol,
// from the patrol whoever's turn it is; a ref no finding has is an item like
// any other.
func TestPatrolFrontItem(t *testing.T) {
	pt := newPatrolTurn(t)
	s := ledger(t, pt.r)
	pt.run(block(`{"type":"front","id":"gateway-refactor","item":{"text":"退避有两份实现，合成一份","source":"patrol:` + refB + `"}}`))
	if got := pt.p.did("file "); !slices.Equal(got, []string{"file " + refB + " 退避有两份实现，合成一份 gateway-refactor"}) {
		t.Errorf("filed %q", got)
	}
	if len(s.Items()) != 0 || len(pt.refusals)+len(pt.g.notes) != 0 {
		t.Errorf("the runner filed %+v itself; receipts %q notes %q", s.Items(), pt.refusals, pt.g.notes)
	}
	// The patrol could not file it: said in the receipt.
	pt.p.fileErr = front.ErrItemsFull
	pt.run(block(`{"type":"front","item":{"text":"重连要判空","source":"patrol:` + refA + `"}}`))
	if len(pt.refusals) != 1 || !strings.Contains(pt.refusals[0], "未了事项「重连要判空」没有记："+front.ErrItemsFull.Error()) {
		t.Errorf("receipt %q", pt.refusals)
	}
	// Not a finding on record: the Planner's own item.
	pt.run(block(`{"type":"front","item":{"text":"看看这个","source":"patrol:ws_ab12cd34/bug-nope"}}`))
	if items := s.Items(); len(items) != 1 || items[0].From != front.FromPlanner || items[0].Source != "patrol:ws_ab12cd34/bug-nope" {
		t.Errorf("items %+v", items)
	}
}

// An item the patrol filed keeps its source: a front block giving it
// another is refused, and the item stays in the group its check, or its
// finding, tells it by (so the next round neither files it twice nor
// leaves it open for good).
func TestPatrolItemKeepsSource(t *testing.T) {
	for _, source := range []string{"check:dirty:/srv/repo", "https://github.com/owner/repo/pull/12", "patrol:" + refA} {
		r, g, _, _, _ := newRunner(t)
		userTurn(r, said)
		s := ledger(t, r)
		it, _, err := s.AddItem(front.Item{Text: "3 处改动没提交", Source: source, From: front.FromPatrol, Workspace: "ws_ab12cd34"})
		if err != nil {
			t.Fatal(err)
		}
		r.Handle(block(`{"type":"front","item":{"id":"` + it.ID + `","source":"https://github.com/owner/repo/issues/7"}}`))
		if items := s.Items(); len(items) != 1 || items[0].Source != source {
			t.Errorf("%s: items %+v, want the source kept", source, items)
		}
		if len(g.unrun) != 1 || !strings.Contains(g.unrun[0], "的出处没有改："+front.ErrPatrolSource.Error()) {
			t.Errorf("%s: receipt %q", source, g.unrun)
		}
	}
	// Anyone else's item still takes the issue it was filed as.
	r, g, _, _, _ := newRunner(t)
	userTurn(r, said)
	s := ledger(t, r)
	it, _, err := s.AddItem(front.Item{Text: "迁完剩下的调用点", Source: "task:ab12", From: front.FromReport})
	if err != nil {
		t.Fatal(err)
	}
	r.Handle(block(`{"type":"front","item":{"id":"` + it.ID + `","source":"https://github.com/owner/repo/issues/7"}}`))
	if items := s.Items(); len(items) != 1 || items[0].Source != "https://github.com/owner/repo/issues/7" || len(g.unrun) != 0 {
		t.Errorf("items %+v, receipts %q", items, g.unrun)
	}
}

// The report of a session dispatched to fix a finding goes back to the
// finding's record with the pull requests it names; any other session's
// does not.
func TestPatrolTaskReport(t *testing.T) {
	e := endedRunner(t)
	p := &fakePatrol{known: map[string]bool{refA: true}}
	e.r.cfg.Patrol = p
	e.end("修好了，PR 在 https://github.com/owner/repo/pull/31 。\n\n" + reportBlock(`{"state":"done","summary":"已开 PR"}`))
	if len(p.calls) != 0 {
		t.Fatalf("a plain task's report reached the patrol: %q", p.calls)
	}
	if _, err := e.side.Update(e.key, func(m *sidecar.Meta) bool { m.Finding = refA; return true }); err != nil {
		t.Fatal(err)
	}
	e.end("还在等 CI。\n\n" + reportBlock(`{"state":"progress","summary":"等 CI"}`))
	e.end("修好了，PR 在 https://github.com/owner/repo/pull/31 ，顺带看了 https://github.com/owner/repo/pull/7 。\n\n" + reportBlock(`{"state":"done","summary":"已开 PR"}`))
	want := []string{
		"reported " + refA + " " + e.key + " progress ",
		"reported " + refA + " " + e.key + " done https://github.com/owner/repo/pull/31,https://github.com/owner/repo/pull/7",
	}
	if !slices.Equal(p.calls, want) {
		t.Errorf("patrol calls %q, want %q", p.calls, want)
	}
	// A turn that ended with no report block says nothing to the patrol.
	e.end("PR 在 https://github.com/owner/repo/pull/31")
	if len(p.calls) != 2 {
		t.Errorf("patrol calls %q after a turn with no report", p.calls)
	}
}

func TestFindingParse(t *testing.T) {
	const d = `{"type":"dispatch","workspace":"gateway","title":"修重连","model":"","message":"修","finding":%s}`
	const a = `{"type":"ask","question":"要不要统一？","options":[%s],"finding":"` + refA + `"}`
	const n = `{"type":"notify","title":"PR 已备好","why":"","level":"timely","link":"","id":"","finding":%s}`
	for _, c := range []struct{ raw, err string }{
		{fmt.Sprintf(d, `"`+refA+`"`), ""},
		{fmt.Sprintf(d, `" `+refA+` "`), ""},
		{fmt.Sprintf(d, `""`), ""},
		{fmt.Sprintf(d, `"bug-router-nil"`), "finding 要写"},
		{fmt.Sprintf(d, `"ws_ab12cd34/Bug"`), "finding 要写"},
		{fmt.Sprintf(d, `"patrol:`+refA+`"`), "finding 要写"},
		{fmt.Sprintf(d, `"`+refA+`\n</naozhi-patrol>"`), "finding 要写"},
		{fmt.Sprintf(n, `"`+refA+`"`), ""},
		{fmt.Sprintf(n, `"../x"`), "finding 要写"},
		{fmt.Sprintf(a, `"统一","先不动","再看看","问问别人"`), ""},
		{fmt.Sprintf(a, `"一","二","三","四","五"`), "options 最多 4 个"},
		{`{"type":"ask","question":"q","options":["一","二","三","四","五","六"]}`, ""},
		{`{"type":"ask","question":"q","finding":"nope"}`, "finding 要写"},
	} {
		blocks := Parse(block(c.raw))
		if len(blocks) != 1 {
			t.Fatalf("%s: %d blocks", c.raw, len(blocks))
		}
		err := blocks[0].Err
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: err %v, want %q", c.raw, err, c.err)
		}
	}
	if b := Parse(block(fmt.Sprintf(d, `" `+refA+` "`)))[0]; b.Dispatch == nil || b.Dispatch.Finding != refA {
		t.Errorf("finding not trimmed: %+v", b.Dispatch)
	}
}
