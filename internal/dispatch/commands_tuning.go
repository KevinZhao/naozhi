package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/tuningspec"
)

// tuningResetToken is the argument that clears an override so the config
// chain applies again (the dashboard's "恢复默认").
const tuningResetToken = "reset"

// tuningTarget resolves "<value> [agent]" for /model, /effort and /backend:
// the value, and the session key of the agent the chat addresses (general, or
// the planner when the chat is project-bound). ok=false means the agent token
// was unknown and a reply was already sent.
func (d *Dispatcher) tuningTarget(ctx context.Context, msg platform.IncomingMessage, args string, log *slog.Logger) (value, key, agentID string, ok bool) {
	fields := strings.Fields(trimUnicodeSpace(args))
	if len(fields) > 2 {
		d.replyText(ctx, msg, "参数过多。用法：/<命令> [值] [agent]", log)
		return "", "", "", false
	}
	agentID = "general"
	if len(fields) == 2 {
		id, found := d.resolveAgentToken(strings.ToLower(fields[1]))
		if !found {
			d.replyText(ctx, msg, "未知的 agent: "+osutil.SanitizeForLog(fields[1], 64), log)
			return "", "", "", false
		}
		agentID = id
	}
	if len(fields) >= 1 {
		value = fields[0]
	}
	return value, d.keyForChat(msg.Platform, msg.ChatType, msg.ChatID, agentID), agentID, true
}

// defaultBackendID is the first (default) entry of the host's catalogue, or "".
func (d *Dispatcher) defaultBackendID() string {
	if ids := d.caps.BackendIDs(); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// snapshotForKey reads key's live snapshot through VisitSessions; ok=false
// when the key has no session (a pick recorded via /model or /backend before
// the first message is parked in the router and applies on spawn).
func (d *Dispatcher) snapshotForKey(key string) (snap sessionview.SessionSnapshot, ok bool) {
	d.router.VisitSessions(func(s sessionview.SessionSnapshot) bool {
		if s.Key != key {
			return true
		}
		snap, ok = s, true
		return false
	})
	return snap, ok
}

// tuningStateText renders the current state line /model and /effort share.
func tuningStateText(snap sessionview.SessionSnapshot, found bool, defaultBackend string) string {
	if !found {
		return "当前会话尚未创建（默认 backend: " + osutil.SanitizeForLog(defaultBackend, 64) + "）；已设置的覆盖会在首条消息时生效。"
	}
	var b strings.Builder
	b.WriteString("backend: " + osutil.SanitizeForLog(snap.Backend, 64))
	model := snap.Model
	if model == "" {
		model = snap.TuningModel
	}
	if model == "" {
		b.WriteString("\nmodel: （配置默认）")
	} else {
		b.WriteString("\nmodel: " + osutil.SanitizeForLog(model, 64))
		if snap.TuningModel != "" {
			b.WriteString("（会话覆盖）")
		}
	}
	if snap.TuningEffort != "" {
		b.WriteString("\neffort: " + osutil.SanitizeForLog(snap.TuningEffort, 16) + "（会话覆盖）")
	} else {
		b.WriteString("\neffort: （配置默认）")
	}
	return b.String()
}

// appliedText turns a session.TuningApplied* mode into the user's reply.
func appliedText(what, value, appliedVia string) string {
	shown := osutil.SanitizeForLog(value, 64)
	if value == "" {
		shown = "配置默认"
	}
	switch appliedVia {
	case sessionview.TuningAppliedRPC:
		return what + " 已切换为 " + shown + "，立即生效。"
	case sessionview.TuningAppliedRespawn:
		return what + " 已设为 " + shown + "，下一条消息起生效（上下文保留）。"
	default:
		return what + " 已记录为 " + shown + "，会话启动时生效。"
	}
}

// handleModelCommand implements /model [name|reset] [agent].
func (d *Dispatcher) handleModelCommand(ctx context.Context, msg platform.IncomingMessage, args string, log *slog.Logger) {
	value, key, _, ok := d.tuningTarget(ctx, msg, args, log)
	if !ok {
		return
	}
	if value == "" {
		snap, found := d.snapshotForKey(key)
		d.replyText(ctx, msg, tuningStateText(snap, found, d.defaultBackendID())+
			"\n用法：/model <名称> [agent]，/model reset 恢复默认", log)
		return
	}
	if strings.EqualFold(value, tuningResetToken) {
		value = ""
	} else if err := tuningspec.ValidateModel("model", value); err != nil {
		d.replyText(ctx, msg, "无效的模型名: "+osutil.SanitizeForLog(value, 64), log)
		return
	}
	d.applyTuning(ctx, msg, key, "模型", value, &value, nil, log)
}

// handleEffortCommand implements /effort <tier|reset> [agent].
func (d *Dispatcher) handleEffortCommand(ctx context.Context, msg platform.IncomingMessage, args string, log *slog.Logger) {
	value, key, _, ok := d.tuningTarget(ctx, msg, args, log)
	if !ok {
		return
	}
	tiers := slices.Sorted(maps.Keys(tuningspec.EffortTiers))
	if value == "" {
		snap, found := d.snapshotForKey(key)
		d.replyText(ctx, msg, tuningStateText(snap, found, d.defaultBackendID())+
			"\n用法：/effort <"+strings.Join(tiers, "|")+"> [agent]，/effort reset 恢复默认", log)
		return
	}
	value = strings.ToLower(value)
	if strings.EqualFold(value, tuningResetToken) {
		value = ""
	} else if err := tuningspec.ValidateEffort("effort", value); err != nil {
		d.replyText(ctx, msg, "无效的 effort 档位，可选: "+strings.Join(tiers, ", "), log)
		return
	}
	d.applyTuning(ctx, msg, key, "effort", value, nil, &value, log)
}

// applyTuning runs SetSessionTuning and maps its outcome to a reply.
func (d *Dispatcher) applyTuning(ctx context.Context, msg platform.IncomingMessage, key, what, value string, model, effort *string, log *slog.Logger) {
	appliedVia, err := d.router.SetSessionTuning(ctx, key, model, effort)
	switch {
	case err == nil:
		d.replyText(ctx, msg, appliedText(what, value, appliedVia), log)
		log.Info("session tuning set by user", "what", what, "applied_via", appliedVia)
	case errors.Is(err, sessionview.ErrTuningEffortUnsupported):
		d.replyText(ctx, msg, "当前后端不支持 effort 档位。", log)
	case errors.Is(err, clierr.ErrSetModelRejected):
		// CLI text is sanitized at the protocol layer before entering the chain.
		d.replyText(ctx, msg, "CLI 拒绝切换: "+err.Error(), log)
	default:
		d.replyText(ctx, msg, "设置失败，请稍后重试。", log)
		log.Warn("session tuning failed", "what", what, "err", err)
	}
}

// handleBackendCommand implements /backend [id|reset] [agent]. The pick applies
// to the next spawn only, so a live session keeps its CLI until /new.
func (d *Dispatcher) handleBackendCommand(ctx context.Context, msg platform.IncomingMessage, args string, log *slog.Logger) {
	value, key, agentID, ok := d.tuningTarget(ctx, msg, args, log)
	if !ok {
		return
	}
	ids := d.caps.BackendIDs()
	if value == "" {
		cur := d.defaultBackendID()
		if snap, found := d.snapshotForKey(key); found && snap.Backend != "" {
			cur = snap.Backend
		}
		d.replyText(ctx, msg, "当前 backend: "+osutil.SanitizeForLog(cur, 64)+
			"\n可用: "+strings.Join(ids, ", ")+
			"\n用法：/backend <id> [agent]，/backend reset 恢复默认", log)
		return
	}
	if strings.EqualFold(value, tuningResetToken) {
		value = ""
	} else if !slices.Contains(ids, value) {
		d.replyText(ctx, msg, "未知的 backend: "+osutil.SanitizeForLog(value, 64)+"\n可用: "+strings.Join(ids, ", "), log)
		return
	}
	d.router.SetSessionBackend(key, value)
	shown := osutil.SanitizeForLog(value, 64)
	if value == "" {
		shown = "配置默认"
	}
	reply := "backend 已设为 " + shown + "，"
	if snap, found := d.snapshotForKey(key); found {
		newArg := ""
		if agentID != "general" {
			newArg = " " + agentID
		}
		reply += "当前会话仍在使用 " + osutil.SanitizeForLog(snap.Backend, 64) + "；发送 /new" + newArg + " 后生效。"
	} else {
		reply += "会话启动时生效。"
	}
	d.replyText(ctx, msg, reply, log)
	log.Info("session backend set by user", "backend", value, "agent", agentID)
}
