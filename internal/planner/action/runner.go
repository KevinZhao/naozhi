package action

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Decision handlers the runner registers with the inbox.
const (
	HandlerDispatch = "dispatch"
	HandlerAsk      = "ask"
)

const (
	// queueLen bounds the replies waiting to be run; past it a reply's
	// blocks are dropped (logged), never the read loop blocked.
	queueLen = 16
	// spawnTimeout bounds an approved dispatch's spawn.
	spawnTimeout = time.Minute
	// rawRunes caps a refused block as shown in the conversation.
	rawRunes = 600
	// messagePreviewRunes caps the first message on a confirmation card.
	messagePreviewRunes = 300
)

// Spawn is an approved dispatch as the gateway starts it.
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
	// Spawn starts the dispatched session key and sends it the first
	// message.
	Spawn func(ctx context.Context, key string, s Spawn) error
	// Note writes text into the Planner conversation for the user; it never
	// reaches the Planner.
	Note func(text string)
	// Tell sends text to the Planner as the gateway's receipt.
	Tell func(text string) error
	// Ctx bounds approved dispatches' spawns; nil is Background.
	Ctx context.Context
}

// Runner runs the action blocks of the Planner's replies and applies the
// user's answers to the cards they open.
type Runner struct {
	cfg Config
	ch  chan string
}

// dispatchCard is a dispatch card's decision payload: the request as
// resolved when it was proposed.
type dispatchCard struct {
	Workspace     string `json:"workspace"`
	WorkspaceName string `json:"workspace_name"`
	Title         string `json:"title"`
	Model         string `json:"model,omitempty"`
	Message       string `json:"message"`
}

type askCard struct {
	Question string `json:"question"`
}

// New returns a Runner and registers its decision handlers with cfg.Inbox.
func New(cfg Config) *Runner {
	if cfg.Ctx == nil {
		cfg.Ctx = context.Background()
	}
	r := &Runner{cfg: cfg, ch: make(chan string, queueLen)}
	if cfg.Inbox != nil {
		cfg.Inbox.HandleDecisions(HandlerDispatch, r.decideDispatch)
		cfg.Inbox.HandleDecisions(HandlerAsk, r.decideAsk)
	}
	return r
}

// Result is the router's result sink: it queues key's reply when key is the
// Planner and the reply holds an action block. It never blocks.
func (r *Runner) Result(key, text string) {
	if key != r.cfg.Planner || !strings.Contains(text, Fence) {
		return
	}
	select {
	case r.ch <- text:
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
		case text := <-r.ch:
			r.Handle(text)
		}
	}
}

// Handle runs text's action blocks in order.
func (r *Runner) Handle(text string) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("planner actions: panicked", "panic", p)
		}
	}()
	for _, b := range Parse(text) {
		if b.Err != nil {
			// A note is one line: the block's whitespace folds to spaces.
			r.note("动作块格式不对，没有执行：" + b.Err.Error() + "。原文：" + textutil.TruncateRunes(strings.Join(strings.Fields(b.Raw), " "), rawRunes))
			continue
		}
		var err error
		switch {
		case b.Dispatch != nil:
			err = r.propose(*b.Dispatch)
		case b.Promise != nil:
			err = r.promise(*b.Promise)
		case b.Ask != nil:
			err = r.ask(*b.Ask)
		}
		if err != nil {
			r.note("动作块没有执行：" + err.Error())
		}
	}
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

// propose opens the confirmation card of a dispatch.
func (r *Runner) propose(d Dispatch) error {
	if r.cfg.Inbox == nil || r.cfg.Spawn == nil {
		return errors.New("派活没有接通")
	}
	w, _, err := r.resolve(d.Workspace)
	if err != nil {
		return err
	}
	card := dispatchCard{Workspace: w.ID, WorkspaceName: w.Name, Title: d.Title, Model: d.Model, Message: d.Message}
	payload, err := json.Marshal(card)
	if err != nil {
		return err
	}
	it, _ := r.cfg.Inbox.Add(notify.Item{
		Kind:     notify.KindDecision,
		Unit:     r.cfg.Planner,
		WS:       w.ID,
		Title:    "派出会话：" + d.Title,
		Why:      "工作区 " + w.Name + " · 模型 " + modelLabel(d.Model) + " · 第一条消息：" + textutil.TruncateRunes(d.Message, messagePreviewRunes),
		Source:   "dispatch:" + requestID(),
		Decision: &notify.Decision{Handler: HandlerDispatch, Payload: payload, Actions: []notify.Action{notify.ActionApprove, notify.ActionReject}},
	})
	r.note("待你确认：派出「" + d.Title + "」（工作区 " + w.Name + "，模型 " + modelLabel(d.Model) + "）。卡片 " + it.ID + " 在「等你」里，批准后才会拉起。")
	return nil
}

func modelLabel(m string) string {
	if m == "" {
		return "默认"
	}
	return m
}

// decideDispatch applies the user's answer to a dispatch card. The card's
// workspace is resolved again: it may have moved since the proposal.
func (r *Runner) decideDispatch(it notify.Item, a notify.Action) error {
	var c dispatchCard
	if it.Decision == nil || json.Unmarshal(it.Decision.Payload, &c) != nil || c.Title == "" {
		return errors.New("dispatch card: unreadable payload")
	}
	if a == notify.ActionReject {
		r.note("你驳回了派出「" + c.Title + "」。")
		r.tell("用户驳回了你提议派出的「" + c.Title + "」（卡片 " + it.ID + "），没有拉起会话。")
		return nil
	}
	_, dir, err := r.resolve(c.Workspace)
	if err != nil {
		r.note("派出「" + c.Title + "」失败：" + err.Error())
		return err
	}
	key := sidecar.TaskKey()
	if err := r.cfg.Sessions.Put(sidecar.Meta{
		Key: key, Purpose: sidecar.PurposeTask, Parent: r.cfg.Planner, Title: c.Title, Workspace: c.Workspace,
		Dir: dir, Model: c.Model,
	}); err != nil {
		r.note("派出「" + c.Title + "」失败：记不下会话信息（" + err.Error() + "）")
		return err
	}
	ctx, cancel := context.WithTimeout(r.cfg.Ctx, spawnTimeout)
	defer cancel()
	if err := r.cfg.Spawn(ctx, key, Spawn{Dir: dir, Title: c.Title, Model: c.Model, Message: c.Message}); err != nil {
		r.cfg.Sessions.Remove(key)
		r.note("派出「" + c.Title + "」失败：" + err.Error())
		return err
	}
	r.note("已派出：" + c.Title + "（" + key + "）")
	r.tell("已派出：" + c.Title + "（" + key + "），工作区 " + c.WorkspaceName + "。它的跨会话名字是「" + c.Title + "」（重名时 CC 会加短 ID，以 ListAgents 为准）。用 notify_when_idle 订阅它，做完后在对话里告诉用户结果。")
	return nil
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

// ask puts the Planner's question into "等你".
func (r *Runner) ask(q Ask) error {
	if r.cfg.Inbox == nil {
		return errors.New("等你没有接通")
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
