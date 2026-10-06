package dispatch

import (
	"context"
	"expvar"
	"fmt"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/imbudget"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Usage gates (docs/rfc/im-usage-limits.md §3.4): a per-sender rate limit
// runs before any command so a flood of /help is a flood; the per-chat
// budget runs after command dispatch so a chat at its limit can still
// /stop, /new or /model its way out.

var (
	// dispatchRateLimitedTotal counts IM messages dropped by the per-sender
	// rate limit, keyed by platform.
	dispatchRateLimitedTotal = expvar.NewMap("naozhi_dispatch_ratelimit_total")
	// dispatchBudgetBlockTotal counts IM messages refused at the chat
	// budget, keyed by platform.
	dispatchBudgetBlockTotal = expvar.NewMap("naozhi_dispatch_budget_block_total")
	// dispatchBudgetWarnTotal counts once-per-window budget warnings sent.
	dispatchBudgetWarnTotal = expvar.NewInt("naozhi_dispatch_budget_warn_total")
	// dispatchBudgetLedgerErrorTotal counts checks that passed because the
	// ledger read failed.
	dispatchBudgetLedgerErrorTotal = expvar.NewInt("naozhi_dispatch_budget_ledger_error_total")
)

// SetBudgetGate replaces the per-chat budget gate; nil turns it off.
func (d *Dispatcher) SetBudgetGate(g *imbudget.Gate) { d.budget.Store(g) }

// SetUserLimiter replaces the per-sender rate limiter; nil turns it off.
func (d *Dispatcher) SetUserLimiter(l *ratelimit.Limiter) { d.userRate.Store(l) }

// rateLimitOK reports whether msg's sender is within their message rate. A
// hit is dropped silently (a reply would reward the flood) except for one
// notice per sender per denyReplyWindow. Empty UserID is not limited: the
// access policy already refuses it wherever a platform is not fully open.
func (d *Dispatcher) rateLimitOK(ctx context.Context, msg platform.IncomingMessage, lg *slog.Logger) bool {
	l := d.userRate.Load()
	if l == nil || msg.UserID == "" {
		return true
	}
	key := msg.Platform + "\x00" + msg.UserID
	if l.Allow(key) {
		return true
	}
	dispatchRateLimitedTotal.Add(osutil.SanitizeForLog(msg.Platform, 32), 1)
	lg.Debug("im message rate limited")
	if d.denyReplies.allow("rate\x00"+key, time.Now()) {
		d.replyText(ctx, msg, "发送过快，请稍候再试。", lg)
	}
	return false
}

// budgetOK reports whether msg's chat may start another turn. A refusal is
// answered at most once per chat per denyReplyWindow; a warning once per
// budget window (the gate tracks that).
func (d *Dispatcher) budgetOK(ctx context.Context, msg platform.IncomingMessage, lg *slog.Logger) bool {
	g := d.budget.Load()
	if g == nil {
		return true
	}
	chatKey := sessionkey.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
	dec := g.Check(chatKey)
	if dec.LedgerError {
		dispatchBudgetLedgerErrorTotal.Add(1)
		lg.Warn("im budget check skipped: cost ledger read failed")
		return true
	}
	if dec.Block {
		dispatchBudgetBlockTotal.Add(osutil.SanitizeForLog(msg.Platform, 32), 1)
		lg.Info("im budget exhausted", "spent_usd", dec.Spent, "limit_usd", dec.Limit)
		if d.denyReplies.allow("budget\x00"+chatKey, time.Now()) {
			d.replyText(ctx, msg, fmt.Sprintf(
				"本会话 24 小时内的费用预算已用尽（$%.2f / $%.2f），请联系管理员或等待额度恢复。",
				dec.Spent, dec.Limit), lg)
		}
		return false
	}
	if dec.Warn {
		dispatchBudgetWarnTotal.Add(1)
		lg.Info("im budget warning", "spent_usd", dec.Spent, "limit_usd", dec.Limit)
		d.replyText(ctx, msg, fmt.Sprintf("提示：本会话 24 小时内已使用 $%.2f / $%.2f。", dec.Spent, dec.Limit), lg)
	}
	return true
}
