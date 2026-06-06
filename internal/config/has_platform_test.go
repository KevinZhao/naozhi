package config

import (
	"testing"
)

// TestConfig_HasPlatform pins [R20260602-ARCH-1]: hasPlatform must return true
// for each known platform when a non-nil config section is present, false when
// the section is nil, and false for unknown platform names.
func TestConfig_HasPlatform(t *testing.T) {
	t.Parallel()

	feishu := &FeishuConfig{}
	slack := &SlackConfig{}
	discord := &DiscordConfig{}
	weixin := &WeixinConfig{}

	tests := []struct {
		name     string
		cfg      Config
		platform string
		want     bool
	}{
		// configured → true
		{"feishu_configured", Config{Platforms: PlatformConfigs{Feishu: feishu}}, "feishu", true},
		{"slack_configured", Config{Platforms: PlatformConfigs{Slack: slack}}, "slack", true},
		{"discord_configured", Config{Platforms: PlatformConfigs{Discord: discord}}, "discord", true},
		{"weixin_configured", Config{Platforms: PlatformConfigs{Weixin: weixin}}, "weixin", true},
		// nil section → false
		{"feishu_nil", Config{}, "feishu", false},
		{"slack_nil", Config{}, "slack", false},
		{"discord_nil", Config{}, "discord", false},
		{"weixin_nil", Config{}, "weixin", false},
		// unknown → false
		{"unknown_telegram", Config{Platforms: PlatformConfigs{Feishu: feishu}}, "telegram", false},
		{"unknown_empty", Config{}, "", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.cfg.hasPlatform(tt.platform)
			if got != tt.want {
				t.Errorf("hasPlatform(%q) = %v, want %v", tt.platform, got, tt.want)
			}
		})
	}
}
