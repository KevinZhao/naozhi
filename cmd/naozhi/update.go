package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/limits"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/selfupdate"
)

// startUpdateChecker launches the background auto-update checker goroutine.
// It bridges config + the platform map into the platform-agnostic
// selfupdate.Checker via a best-effort NotifyFunc.
func startUpdateChecker(ctx context.Context, cfg *config.Config, platforms map[string]platform.Platform) {
	checker := selfupdate.NewChecker(selfupdate.CheckerConfig{
		CurrentVersion: version,
		Mode:           selfupdate.ParseMode(cfg.Update.Mode),
		Interval:       cfg.UpdateInterval(),
		CheckOnStart:   cfg.Update.CheckOnStart,
		Notify:         updateNotifyFunc(ctx, cfg, platforms),
	})
	if checker == nil {
		return
	}
	go checker.Run(ctx)
}

// updateNotifyFunc returns a NotifyFunc that delivers update notices to the
// configured update.notify target, or nil when no target is set. Failures are
// logged and swallowed — a notice is never load-bearing.
func updateNotifyFunc(ctx context.Context, cfg *config.Config, platforms map[string]platform.Platform) selfupdate.NotifyFunc {
	plat := cfg.Update.Notify.Platform
	chatID := cfg.Update.Notify.ChatID
	if plat == "" || chatID == "" {
		return nil
	}
	return func(text string) {
		p := platforms[plat]
		if p == nil {
			slog.Warn("auto-update notify: platform not found", "platform", plat)
			return
		}
		// Bound the delivery so a wedged platform call can't pin the
		// checker goroutine; independent of the parent check cycle ctx.
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := platform.ReplyWithRetry(sendCtx, p, platform.OutgoingMessage{
			ChatID: chatID,
			Text:   text,
		}, limits.PlatformReplyMaxAttempts); err != nil {
			slog.Warn("auto-update notify: delivery failed", "platform", plat, "err", err)
		}
	}
}
