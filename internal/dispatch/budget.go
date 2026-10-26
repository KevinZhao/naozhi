package dispatch

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/naozhi/naozhi/internal/agentroute"
	"github.com/naozhi/naozhi/internal/budget"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// budgetReplyWindow is how long a refused chat waits for the next "budget
// spent" reply; the messages in between are dropped silently.
const budgetReplyWindow = time.Minute

// BudgetGate is the daily spend check an IM turn passes before it is
// submitted; satisfied by *budget.Gate.
type BudgetGate interface {
	CheckKey(sessionKey string) budget.Verdict
	Once(n budget.Notice, s budget.Subject) bool
}

// admitBudget reports whether key, the session msg routes to, may take a
// turn under today's budget. Commands never get here, so /stop and /new work
// in a refused chat. A refused message gets no turn,
// and the chat is told at most once per budgetReplyWindow.
func (d *Dispatcher) admitBudget(ctx context.Context, msg platform.IncomingMessage, key string, lg *slog.Logger) bool {
	if d.budget == nil {
		return true
	}
	v := d.budget.CheckKey(key)
	if !v.Blocked {
		return true
	}
	chat := sessionkey.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
	if !d.budgetReplies.allow(chat, time.Now()) {
		lg.DebugContext(ctx, "im turn refused: daily budget spent")
		return false
	}
	lg.InfoContext(ctx, "im turn refused: daily budget spent",
		"subject", string(v.Subject), "spent_usd", v.Spent, "limit_usd", v.Limit)
	d.replyText(ctx, msg, "今日费用预算已用尽（"+imBudgetScope(v.Subject)+" "+v.Usage()+"），"+
		v.ResetAt.Format("01-02 15:04")+" 重置；在此之前新消息不会处理。", lg)
	return false
}

// peekBudget is admitBudget for Admit, before msg's media is fetched. The
// budget subject is the parent chat's, or a bound project's planner for the
// general agent, so the agent is all of the handler's routing it needs.
func (d *Dispatcher) peekBudget(ctx context.Context, msg platform.IncomingMessage, trimmed string, lg *slog.Logger) bool {
	agentID, _ := agentroute.ResolveAgent(trimmed, d.agentCommands)
	key, _ := d.resolver.ResolveForChat(msg.Platform, msg.ChatType, msg.ChatID, agentID)
	return d.admitBudget(ctx, msg, key, lg)
}

// budgetWarnLine is the line a reply on key ends with when its chat, project
// or the machine has passed warn_ratio or its cap: once a day per subject and
// level, "" otherwise.
func (d *Dispatcher) budgetWarnLine(key string) string {
	if d.budget == nil {
		return ""
	}
	v := d.budget.CheckKey(key)
	if !v.Warn {
		return ""
	}
	n := budget.NoticeWarn
	if v.Over {
		n = budget.NoticeOver
	}
	if !d.budget.Once(n, v.Subject) {
		return ""
	}
	usage := "（" + imBudgetScope(v.Subject) + " " + v.Usage() + "）"
	switch {
	case v.Blocked:
		return "\n\n⚠️ 今日费用预算已用尽" + usage + "，" + v.ResetAt.Format("01-02 15:04") + " 前新消息不会处理"
	case v.Over:
		return "\n\n⚠️ 今日费用已超出预算" + usage
	}
	return "\n\n⚠️ 今日费用已达预算的 " + strconv.Itoa(int(v.Spent/v.Limit*100)) + "%" + usage
}

// imBudgetScope names the subject an IM verdict is about.
func imBudgetScope(s budget.Subject) string {
	switch s.Kind() {
	case "chat":
		return "本会话"
	case "project":
		return "项目 " + s.Name()
	case "job":
		return "定时任务"
	}
	return "本机"
}
