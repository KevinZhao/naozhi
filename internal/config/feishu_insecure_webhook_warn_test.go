package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureSlog installs a JSONHandler over the package default for the
// duration of fn and returns everything written. No t.Parallel in callers:
// SetDefault mutates the process-global logger, which races concurrent
// tests that log through slog.Default() (cf. apierr R20260603030037-GO-1).
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	fn()
	return buf.String()
}

// TestValidateConfig_FeishuInsecureWebhookWarns pins #1656: enabling
// allow_insecure_webhook without an encrypt_key must surface a warning at
// validation time (not only at Start()), so operators copying a template
// learn the webhook lacks HMAC verification before the daemon is up.
func TestValidateConfig_FeishuInsecureWebhookWarns(t *testing.T) {
	// No t.Parallel: see captureSlog — this test swaps slog.Default().
	cfg := &Config{
		Platforms: PlatformConfigs{
			Feishu: &FeishuConfig{
				AppID:                "app",
				AppSecret:            "secret",
				ConnectionMode:       "webhook",
				VerificationToken:    "vtok",
				AllowInsecureWebhook: true,
				// EncryptKey intentionally empty → token-only insecure mode.
			},
		},
	}

	out := captureSlog(t, func() {
		if err := validateConfig(cfg); err != nil {
			t.Fatalf("validateConfig() unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "token-only") {
		t.Errorf("expected token-only insecure-mode warning, got log: %q", out)
	}
}

// TestValidateConfig_FeishuSecureWebhookNoWarn confirms the warning is NOT
// emitted when encrypt_key is configured, even with allow_insecure_webhook
// set — the HMAC path is the recommended secure config.
func TestValidateConfig_FeishuSecureWebhookNoWarn(t *testing.T) {
	// No t.Parallel: see captureSlog.
	cfg := &Config{
		Platforms: PlatformConfigs{
			Feishu: &FeishuConfig{
				AppID:                "app",
				AppSecret:            "secret",
				ConnectionMode:       "webhook",
				EncryptKey:           "ekey",
				AllowInsecureWebhook: true,
			},
		},
	}

	out := captureSlog(t, func() {
		if err := validateConfig(cfg); err != nil {
			t.Fatalf("validateConfig() unexpected error: %v", err)
		}
	})

	if strings.Contains(out, "token-only") {
		t.Errorf("did not expect insecure-mode warning when encrypt_key set, got log: %q", out)
	}
}
