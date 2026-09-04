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

// newUpdateChecker builds the auto-update checker without starting it, so the
// same instance reaches both the HTTP layer (Checker.CheckNow) and the
// background loop. Returns nil when the config is unusable.
func newUpdateChecker(ctx context.Context, cfg *config.Config, platforms map[string]platform.Platform, status *selfupdate.Status) *selfupdate.Checker {
	return selfupdate.NewChecker(selfupdate.CheckerConfig{
		CurrentVersion: version,
		Mode:           selfupdate.ParseMode(cfg.Update.Mode),
		Interval:       cfg.UpdateInterval(),
		CheckOnStart:   cfg.Update.CheckOnStart,
		Notify:         updateNotifyFunc(ctx, cfg, platforms),
		Status:         status,
	})
}

// startUpdateChecker launches the background polling loop for a checker built
// by newUpdateChecker. A nil checker is a no-op.
func startUpdateChecker(ctx context.Context, checker *selfupdate.Checker) {
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
		// Bounded so a wedged platform call cannot pin the checker goroutine.
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
