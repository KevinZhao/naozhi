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

// TestValidateConfig_FeishuInsecureWebhookWarns pins R20260603-SEC-6 (#1656)
// and #1724: a feishu webhook with allow_insecure_webhook=true and no
// encrypt_key must emit a prominent SECURITY message at config-validation
// time, not silently pass. #1724 upgrades the severity from WARN to ERROR so
// the insecure posture is audited at the highest level. Conversely, secure
// configurations (encrypt_key set, or flag unset) must NOT emit anything.
func TestValidateConfig_FeishuInsecureWebhookWarns(t *testing.T) {
	base := func() *FeishuConfig {
		return &FeishuConfig{
			AppID:          "app",
			AppSecret:      "secret",
			ConnectionMode: "webhook",
		}
	}

	tests := []struct {
		name     string
		mutate   func(*FeishuConfig)
		wantWarn bool
		wantErr  bool
	}{
		{
			name: "insecure_flag_no_encrypt_key_warns",
			mutate: func(f *FeishuConfig) {
				f.AllowInsecureWebhook = true
				f.VerificationToken = "tok"
			},
			wantWarn: true,
		},
		{
			name: "encrypt_key_set_no_warn",
			mutate: func(f *FeishuConfig) {
				f.AllowInsecureWebhook = true
				f.EncryptKey = "ek"
			},
			wantWarn: false,
		},
		{
			name: "flag_unset_with_token_no_warn",
			mutate: func(f *FeishuConfig) {
				f.VerificationToken = "tok"
			},
			wantWarn: false,
		},
		{
			name: "websocket_mode_flag_set_no_warn",
			mutate: func(f *FeishuConfig) {
				f.ConnectionMode = "websocket"
				f.AllowInsecureWebhook = true
			},
			wantWarn: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			fc := base()
			tt.mutate(fc)
			cfg := &Config{Platforms: PlatformConfigs{Feishu: fc}}

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
