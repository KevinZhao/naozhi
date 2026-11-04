package action

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/textutil"
)

// HandlerAsk is the decision handler the runner registers with the inbox.
const HandlerAsk = "ask"

// handlerDispatch named the dispatch confirmation cards before D88; New
// closes the ones a previous run left open, nothing answers them now.
const handlerDispatch = "dispatch"

const (
	// queueLen bounds the replies waiting to be run; past it a reply's
	// blocks are dropped (logged), never the read loop blocked.
	queueLen = 16
	// spawnTimeout bounds a dispatch's spawn.
	spawnTimeout = time.Minute
	// rawRunes caps a refused block as shown in the conversation.
	rawRunes = 600
)

// Spawn is a dispatch as the gateway starts it.
type Spawn struct {
	// Dir is the workspace's directory on this node.
	Dir     string
	Title   string
	Model   string
	Message string
}

// Config wires a Runner.
type Config struct {
	// Planner is the key whose replies are parsed (planner:main).
	Planner    string
	Inbox      *notify.Notifier
	Promises   *promise.Store
	Workspaces *workspace.Registry
	Sessions   *sidecar.Store
	// Tasks applies status blocks; nil refuses them.
	Tasks *task.Board
	// Spawn starts the dispatched session key and sends it the first
	// message.
	Spawn func(ctx context.Context, key string, s Spawn) error
	// Note writes text into the Planner conversation for the user; it never
	// reaches the Planner.
	Note func(text string)
	// Tell sends text to the Planner as the gateway's receipt.
	Tell func(text string) error
	// Reports are the senders (session keys) of the cross-session messages
	// that reached the Planner in (from, to]; nil checks none.
	Reports func(from, to time.Time) []string
	// Ctx bounds dispatches' spawns; nil is Background.
	Ctx context.Context
	// Now is the wall clock; nil is time.Now.
	Now func() time.Time
}

// Runner runs the action blocks of the Planner's replies and applies the
// user's answers to the questions they ask.
type Runner struct {
	cfg Config
	ch  chan reply

	// last is when the previous Planner reply came: the next one's turn ran
	// after it.
	mu   sync.Mutex
	last time.Time

	// doneTold is the tasks whose done block got the reminder of the
	// promises citing them; only the handling goroutine touches it.
	doneTold map[string]bool
}

// reply is one Planner turn's result text and the span its turn ran in.
type reply struct {
	text     string
	from, to time.Time
}

type askCard struct {
	Question string `json:"question"`
}

// New returns a Runner and registers its decision handler with cfg.Inbox.
func New(cfg Config) *Runner {
	if cfg.Ctx == nil {
		cfg.Ctx = context.Background()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// Reports older than the runner (history restored at startup) were the
	// previous run's to check.
	r := &Runner{cfg: cfg, ch: make(chan reply, queueLen), last: cfg.Now(), doneTold: map[string]bool{}}
	if cfg.Inbox != nil {
		cfg.Inbox.HandleDecisions(HandlerAsk, r.decideAsk)
		open, _ := cfg.Inbox.List(0)
		for _, it := range open {
			if it.Decision != nil && it.Decision.Handler == handlerDispatch {
				_ = cfg.Inbox.Dismiss(it.ID)
			}
		}
	}
	return r
}

// Result is the router's result sink: it queues key's reply when key is the
// Planner, for its action blocks and the reports its turn took. It never
// blocks.
func (r *Runner) Result(key, text string) {
	if key != r.cfg.Planner {
		return
	}
	r.mu.Lock()
	rep := reply{text: text, from: r.last, to: r.cfg.Now()}
	r.last = rep.to
	r.mu.Unlock()
	if r.cfg.Reports == nil && !strings.Contains(text, Fence) {
		return
	}
	select {
	case r.ch <- rep:
	default:
		slog.Warn("planner actions: queue full; reply's action blocks dropped")
	}
}

// Run runs the queued replies until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case rep := <-r.ch:
			r.handle(rep)
		}
	}
}

// Handle runs text's action blocks in order.
func (r *Runner) Handle(text string) { r.handle(reply{text: text}) }

func (r *Runner) handle(rep reply) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("planner actions: panicked", "panic", p)
		}
	}()
	statused := map[string]string{} // task or promise → the state a block set
	for _, b := range Parse(rep.text) {
		if b.Err != nil {
			r.refuse(b, b.Err)
			continue
		}
		var err error
		switch {
		case b.Dispatch != nil:
			r.dispatch(*b.Dispatch)
		case b.Promise != nil:
			err = r.promise(*b.Promise)
		case b.Ask != nil:
			err = r.ask(*b.Ask)
		case b.Status != nil:
			statused[b.Status.Task] = b.Status.State
			err = r.status(b)
		}
		if err != nil {
			r.note("动作块没有执行：" + err.Error())
		}
	}
	r.checkReports(rep, statused)
}

// checkReports reminds the Planner of the dispatched sessions that reported
// to it during the turn and that its reply left open: the task still 进行中
// with no status block for it, or the reply marking it done while a promise
// citing it (its key in the text or close_when) stays open with none
// (docs/rfc/planner.md §15.4; D61: the gateway keeps the list honest when the
// model forgets). A done block with no report in the turn reminds of the
// citing promises too, once per task, so they do not wait on another
// report. A doing or failed block is the Planner's answer, so the promises
// waiting on the task are left alone. It runs after the reply's blocks, so
// what they closed is not asked about. The receipt's own turn takes no
// report, so a report gets at most one.
func (r *Runner) checkReports(rep reply, statused map[string]string) {
	if r.cfg.Tasks == nil {
		return
	}
	var reported []string
	if r.cfg.Reports != nil && !rep.to.IsZero() {
		reported = r.cfg.Reports(rep.from, rep.to)
	}
	keys := slices.Clone(reported)
	for _, k := range slices.Sorted(maps.Keys(statused)) {
		if statused[k] == string(task.Done) && !r.doneTold[k] && !slices.Contains(reported, k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	sessions := map[string]task.Task{}
	for _, t := range r.cfg.Tasks.List() {
		if t.Kind == task.KindSession {
			sessions[t.ID] = t
		}
	}
	var open []promise.Promise
	if r.cfg.Promises != nil {
		open, _ = r.cfg.Promises.List(0)
	}
	var lines []string
	seen := map[string]bool{}
	for _, key := range keys {
		t, ok := sessions[key]
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		var ids []string
		for _, p := range open {
			if statused[p.ID] == "" && p.Cites(key) {
				ids = append(ids, p.ID)
			}
		}
		var line string
		switch {
		case t.State == task.Doing && statused[key] == "":
			line = key + "（" + t.Title + "）刚向你汇报，它在任务里仍是进行中；完成了就用 status 标 done，没完成用 doing 并说明原因（" + statusFormat(key) + "）"
			if len(ids) > 0 {
				line += "；相关的承诺 " + strings.Join(ids, "、") + " 也一并处理，task 填承诺 ID"
			}
		case statused[key] == string(task.Done) && len(ids) > 0:
			line = key + "（" + t.Title + "）刚向你汇报"
			if !slices.Contains(reported, key) {
				line = "你刚把 " + key + "（" + t.Title + "）标为已完成"
			}
			r.doneTold[key] = true
			line += "；相关的承诺 " + strings.Join(ids, "、") + " 还没完成，条件满足了就用 status 标 done（" + statusFormat(ids[0]) + "）"
		default:
			continue
		}
		lines = append(lines, line+"。")
	}
	if len(lines) > 0 {
		r.tell(strings.Join(lines, "\n"))
	}
}

// refuse shows a block that is not run, with why.
func (r *Runner) refuse(b Block, why error) {
	// A note is one line: the block's whitespace folds to spaces.
	r.note("动作块格式不对，没有执行：" + why.Error() + "。原文：" + textutil.TruncateRunes(strings.Join(strings.Fields(b.Raw), " "), rawRunes))
}

func (r *Runner) note(text string) {
	if r.cfg.Note != nil {
		r.cfg.Note(text)
	}
}

func (r *Runner) tell(text string) {
	if r.cfg.Tell == nil {
		return
	}
	if err := r.cfg.Tell(text); err != nil {
		slog.Warn("planner actions: receipt not sent to the Planner", "err", err)
	}
}

// resolve finds a workspace's directory on this node.
func (r *Runner) resolve(ref string) (workspace.Workspace, string, error) {
	if r.cfg.Workspaces == nil {
		return workspace.Workspace{}, "", errors.New("没有工作区登记表")
	}
	w, dir, ok := r.cfg.Workspaces.Find(ref)
	switch {
	case !ok:
		return w, "", fmt.Errorf("找不到工作区「%s」（填 ID，或不重名的名字）", ref)
	case dir == "":
		return w, "", fmt.Errorf("工作区「%s」在这台机器上没有目录", w.Name)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || !filepath.IsAbs(dir) {
		return w, "", fmt.Errorf("工作区「%s」的目录 %s 不可用", w.Name, dir)
	}
	return w, dir, nil
}

// dispatch starts a dispatch's task: session at once (D88): no card waits
// on the user, who can stop it from 任务. Either way the Planner gets a
// receipt and the conversation a line.
func (r *Runner) dispatch(d Dispatch) {
	key, wsName, err := r.spawn(d)
	if err != nil {
		r.note("派出「" + d.Title + "」失败：" + err.Error())
		r.tell("派出「" + d.Title + "」失败，没有拉起会话：" + err.Error())
		return
	}
	r.note("已派出：" + d.Title + "（" + key + "）")
	r.tell("已派出：" + d.Title + "（" + key + "），工作区 " + wsName + "。它的跨会话名字是「" + d.Title + "」（重名时 CC 会加短 ID，以 ListAgents 为准）。用 notify_when_idle 订阅它，做完后在对话里告诉用户结果。")
}

// spawn records a new task: session's sidecar and starts it in the
// workspace's directory; a start that fails leaves no sidecar behind.
func (r *Runner) spawn(d Dispatch) (key, wsName string, err error) {
	if r.cfg.Spawn == nil || r.cfg.Sessions == nil {
		return "", "", errors.New("派活没有接通")
	}
	w, dir, err := r.resolve(d.Workspace)
	if err != nil {
		return "", "", err
	}
	key = sidecar.TaskKey()
	if err := r.cfg.Sessions.Put(sidecar.Meta{
		Key: key, Purpose: sidecar.PurposeTask, Parent: r.cfg.Planner, Title: d.Title, Workspace: w.ID,
		Dir: dir, Model: d.Model,
	}); err != nil {
		return "", "", fmt.Errorf("记不下会话信息（%v）", err)
	}
	ctx, cancel := context.WithTimeout(r.cfg.Ctx, spawnTimeout)
	defer cancel()
	if err := r.cfg.Spawn(ctx, key, Spawn{Dir: dir, Title: d.Title, Model: d.Model, Message: d.Message}); err != nil {
		r.cfg.Sessions.Remove(key)
		return "", "", err
	}
	return key, w.Name, nil
}

// promise registers a promise the Planner made.
func (r *Runner) promise(p Promise) error {
	if r.cfg.Promises == nil {
		return errors.New("承诺没有接通")
	}
	if _, err := r.cfg.Promises.Add(promise.Promise{Text: p.Text, Due: p.Due, CloseWhen: p.CloseWhen}); err != nil {
		switch {
		case errors.Is(err, promise.ErrFull):
			return errors.New("未完成的承诺太多了")
		default:
			return fmt.Errorf("登记承诺失败（%v）", err)
		}
	}
	return nil
}

var stateLabels = map[task.State]string{task.Done: "已完成", task.Doing: "进行中", task.Failed: "失败"}

// status sets a task's state as the Planner judged it. A task that does not
// exist is the block's fault, refused like a malformed one.
func (r *Runner) status(b Block) error {
	if r.cfg.Tasks == nil {
		return errors.New("任务没有接通")
	}
	s := *b.Status
	title, ok := r.cfg.Tasks.Title(s.Task)
	if !ok {
		r.refuse(b, fmt.Errorf("找不到任务 %q（填 task: key 或承诺 ID）", s.Task))
		return nil
	}
	state := task.State(s.State)
	switch err := r.cfg.Tasks.Set(s.Task, state, task.ByPlanner, s.Why); {
	case errors.Is(err, task.ErrState):
		r.refuse(b, errors.New("承诺没有失败状态，只能标 done 或 doing"))
		return nil
	case err != nil:
		return fmt.Errorf("改任务状态失败（%v）", err)
	}
	r.note("Planner 判断任务「" + textutil.TruncateRunes(title, 60) + "」" + stateLabels[state] + "：" + s.Why)
	return nil
}

// ask puts the Planner's question into "通知".
func (r *Runner) ask(q Ask) error {
	if r.cfg.Inbox == nil {
		return errors.New("通知没有接通")
	}
	it := notify.Item{
		Kind:   notify.KindQuestion,
		Unit:   r.cfg.Planner,
		Title:  textutil.TruncateRunes(q.Question, 120),
		Why:    "Planner 问你",
		Source: "ask:" + requestID(),
	}
	if len(q.Options) == 0 {
		it.Why = "Planner 问你，请在 Planner 对话里回复，回复后关掉这一条"
	} else {
		payload, err := json.Marshal(askCard{Question: q.Question})
		if err != nil {
			return err
		}
		acts := make([]notify.Action, len(q.Options))
		for i, o := range q.Options {
			acts[i] = notify.Action(o)
		}
		it.Decision = &notify.Decision{Handler: HandlerAsk, Payload: payload, Actions: acts}
	}
	r.cfg.Inbox.Add(it)
	return nil
}

// decideAsk sends the user's answer back to the Planner. Failing to (the
// Planner is stopped) leaves the question open.
func (r *Runner) decideAsk(it notify.Item, a notify.Action) error {
	var c askCard
	if it.Decision == nil || json.Unmarshal(it.Decision.Payload, &c) != nil {
		return errors.New("ask card: unreadable payload")
	}
	if r.cfg.Tell == nil {
		return errors.New("ask card: no way to the Planner")
	}
	return r.cfg.Tell("你问用户的「" + c.Question + "」，用户的回答：" + string(a))
}

// requestID names one card's source, so no two proposals share a card.
func requestID() string {
	var b [6]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return hex.EncodeToString(b[:])
}

// statusFormat spells out the status block for id, so a conversation started
// on an older system prompt still writes it right.
func statusFormat(id string) string {
	return "格式：```naozhi-action\n{\"type\":\"status\",\"task\":\"" + id + "\",\"state\":\"done\",\"why\":\"…\"}\n```"
}
