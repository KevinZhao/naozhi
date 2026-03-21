package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/platform/feishu"
	"github.com/naozhi/naozhi/internal/server"
	"github.com/naozhi/naozhi/internal/session"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	// Setup logging
	level := slog.LevelInfo
	switch cfg.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	// CLI Wrapper
	wrapper := cli.NewWrapper(cfg.CLI.Path)

	// Session Router
	router := session.NewRouter(session.RouterConfig{
		Wrapper:   wrapper,
		MaxProcs:  cfg.Session.MaxProcs,
		TTL:       cfg.ParseTTL(),
		Model:     cfg.CLI.Model,
		ExtraArgs: cfg.CLI.Args,
	})

	// Context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start cleanup loop
	router.StartCleanupLoop(ctx, cfg.ParseTTL()/2)

	// Register platforms
	platforms := make(map[string]platform.Platform)
	if cfg.Platforms.Feishu != nil {
		f := feishu.New(feishu.Config{
			AppID:             cfg.Platforms.Feishu.AppID,
			AppSecret:         cfg.Platforms.Feishu.AppSecret,
			VerificationToken: cfg.Platforms.Feishu.VerificationToken,
			EncryptKey:        cfg.Platforms.Feishu.EncryptKey,
			MaxReplyLen:       cfg.Platforms.Feishu.MaxReplyLength,
		})
		platforms["feishu"] = f
	}

	if len(platforms) == 0 {
		slog.Error("no platforms configured")
		os.Exit(1)
	}

	// Server
	srv := server.New(cfg.Server.Addr, router, platforms)

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		slog.Info("received signal", "signal", sig)
		cancel()
		router.Shutdown()
	}()

	slog.Info("naozhi starting",
		"addr", cfg.Server.Addr,
		"model", cfg.CLI.Model,
		"max_procs", cfg.Session.MaxProcs,
		"platforms", len(platforms),
	)

	if err := srv.Start(ctx); err != nil && err.Error() != "http: Server closed" {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
