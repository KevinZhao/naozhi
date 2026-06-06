package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureSlog redirects the default slog logger to an in-memory buffer for
// the duration of fn, restoring the previous default afterwards. Returns the
// captured text output.
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// TestValidateConfig_FeishuInsecureWebhook pins R20260603-SEC-6 (#1656),
// #1724, and R20260604-SEC-2 (#1735):
//
//   - allow_insecure_webhook=true with no encrypt_key HARD FAILS regardless of
//     the server bind address — a feishu webhook is reachable from the public
//     internet (including loopback binds behind a frp/ngrok/Cloudflare tunnel),
//     so a leaked verification_token enables event forgery everywhere. There is
//     no loopback exemption.
//   - The only escape is NAOZHI_ALLOW_INSECURE_WEBHOOK=true (CI/testing), which
//     downgrades to a prominent SECURITY message at ERROR level (audit).
//   - Secure configurations (encrypt_key set, flag unset, non-webhook mode)
//     neither warn nor fail.
//
// envOptIn sets the escape hatch. addr is varied to prove the verdict does not
// depend on the bind address.
func TestValidateConfig_FeishuInsecureWebhook(t *testing.T) {
	base := func() *FeishuConfig {
		return &FeishuConfig{
			AppID:          "app",
			AppSecret:      "secret",
			ConnectionMode: "webhook",
		}
	}

	tests := []struct {
		name     string
		addr     string
		envOptIn bool
		mutate   func(*FeishuConfig)
		wantWarn bool
		wantErr  bool
	}{
		{
			name: "insecure_public_bind_hard_fails",
			addr: ":8080",
			mutate: func(f *FeishuConfig) {
				f.AllowInsecureWebhook = true
				f.VerificationToken = "tok"
			},
			wantWarn: false, // returns before the slog.Error
			wantErr:  true,
		},
		{
			name: "insecure_loopback_bind_also_hard_fails",
			addr: "127.0.0.1:8080",
			mutate: func(f *FeishuConfig) {
				f.AllowInsecureWebhook = true
				f.VerificationToken = "tok"
			},
			wantWarn: false, // loopback is NOT exempt (tunnel reachability)
			wantErr:  true,
		},
		{
			name:     "insecure_env_opt_in_warns_no_fail",
			addr:     ":8080",
			envOptIn: true,
			mutate: func(f *FeishuConfig) {
				f.AllowInsecureWebhook = true
				f.VerificationToken = "tok"
			},
			wantWarn: true,
			wantErr:  false,
		},
		{
			name: "encrypt_key_set_no_warn_no_fail",
			addr: ":8080",
			mutate: func(f *FeishuConfig) {
				f.AllowInsecureWebhook = true
				f.EncryptKey = "ek"
			},
			wantWarn: false,
			wantErr:  false,
		},
		{
			name: "flag_unset_with_token_no_warn_no_fail",
			addr: ":8080",
			mutate: func(f *FeishuConfig) {
				f.VerificationToken = "tok"
			},
			wantWarn: false,
			wantErr:  false,
		},
		{
			name: "websocket_mode_flag_set_no_warn_no_fail",
			addr: ":8080",
			mutate: func(f *FeishuConfig) {
				f.ConnectionMode = "websocket"
				f.AllowInsecureWebhook = true
			},
			wantWarn: false,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if tt.envOptIn {
				t.Setenv("NAOZHI_ALLOW_INSECURE_WEBHOOK", "true")
			} else {
				// Ensure a leaked env from the parent process can't mask a
				// hard-fail expectation.
				t.Setenv("NAOZHI_ALLOW_INSECURE_WEBHOOK", "")
			}
			fc := base()
			tt.mutate(fc)
			cfg := &Config{Platforms: PlatformConfigs{Feishu: fc}}
			cfg.Server.Addr = tt.addr

			var err error
			out := captureSlog(t, func() {
				err = validateConfig(cfg)
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateConfig err = %v, wantErr %v", err, tt.wantErr)
			}
			gotWarn := strings.Contains(out, "allow_insecure_webhook") &&
				strings.Contains(out, "SECURITY")
			if gotWarn != tt.wantWarn {
				t.Fatalf("warn emitted = %v, want %v; log = %q", gotWarn, tt.wantWarn, out)
			}
			// #1724: when emitted, the severity must be ERROR (not WARN) so the
			// insecure posture is audited at the highest level.
			if tt.wantWarn && !strings.Contains(out, "level=ERROR") {
				t.Fatalf("expected level=ERROR in log, got %q", out)
			}
		})
	}
}
