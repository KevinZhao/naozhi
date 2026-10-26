package config

import (
	"fmt"
	"net"
	"net/url"
)

type PlatformConfigs struct {
	Feishu  *FeishuConfig  `yaml:"feishu"`
	Slack   *SlackConfig   `yaml:"slack"`
	Discord *DiscordConfig `yaml:"discord"`
	Weixin  *WeixinConfig  `yaml:"weixin"`
}

// hasPlatform reports whether the named platform has a configured section;
// unknown names are false rather than silently accepted.
func (c *Config) hasPlatform(name string) bool {
	switch name {
	case "feishu":
		return c.Platforms.Feishu != nil
	case "slack":
		return c.Platforms.Slack != nil
	case "discord":
		return c.Platforms.Discord != nil
	case "weixin":
		return c.Platforms.Weixin != nil
	default:
		return false
	}
}

type FeishuConfig struct {
	AppID             string `yaml:"app_id"`
	AppSecret         string `yaml:"app_secret"`
	VerificationToken string `yaml:"verification_token"`
	EncryptKey        string `yaml:"encrypt_key"`
	MaxReplyLength    int    `yaml:"max_reply_length"`
}

type SlackConfig struct {
	BotToken       string `yaml:"bot_token"`
	AppToken       string `yaml:"app_token"` // xapp- token for Socket Mode
	MaxReplyLength int    `yaml:"max_reply_length"`
}

type DiscordConfig struct {
	BotToken       string `yaml:"bot_token"`
	MaxReplyLength int    `yaml:"max_reply_length"`
}

type WeixinConfig struct {
	Token          string `yaml:"token"`
	BaseURL        string `yaml:"base_url"`
	MaxReplyLength int    `yaml:"max_reply_length"`
}

type TranscribeConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Region   string `yaml:"region"`
	Language string `yaml:"language"` // BCP-47, default: zh-CN
}

// validatePlatforms checks the four platform blocks: an unexpanded ${VAR} in a
// credential (which would reach the API verbatim) and the per-platform required
// fields. Split out of validateConfig (#2710 J11) with its order preserved.
func validatePlatforms(cfg *Config) error {
	if cfg.Platforms.Feishu != nil {
		if containsEnvPlaceholder(cfg.Platforms.Feishu.AppID) || containsEnvPlaceholder(cfg.Platforms.Feishu.AppSecret) {
			return fmt.Errorf("feishu app_id or app_secret contains unexpanded ${VAR} — check environment variables")
		}
		if containsEnvPlaceholder(cfg.Platforms.Feishu.VerificationToken) {
			return fmt.Errorf("feishu verification_token contains unexpanded ${VAR} — check environment variables")
		}
		if containsEnvPlaceholder(cfg.Platforms.Feishu.EncryptKey) {
			return fmt.Errorf("feishu encrypt_key contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Feishu.AppID == "" || cfg.Platforms.Feishu.AppSecret == "" {
			return fmt.Errorf("feishu app_id and app_secret are required")
		}
	}
	if cfg.Platforms.Slack != nil {
		if containsEnvPlaceholder(cfg.Platforms.Slack.BotToken) {
			return fmt.Errorf("slack bot_token contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Slack.BotToken == "" {
			return fmt.Errorf("slack bot_token is required")
		}
	}
	if cfg.Platforms.Discord != nil {
		if containsEnvPlaceholder(cfg.Platforms.Discord.BotToken) {
			return fmt.Errorf("discord bot_token contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Discord.BotToken == "" {
			return fmt.Errorf("discord bot_token is required")
		}
	}
	if cfg.Platforms.Weixin != nil {
		if containsEnvPlaceholder(cfg.Platforms.Weixin.Token) {
			return fmt.Errorf("weixin token contains unexpanded ${VAR} — check environment variables")
		}
		if cfg.Platforms.Weixin.Token == "" {
			return fmt.Errorf("weixin token is required")
		}
		// base_url flows into every platform HTTP call; pointing it at IMDS or
		// an internal service would turn long-poll/send into SSRF.
		if bu := cfg.Platforms.Weixin.BaseURL; bu != "" {
			u, err := url.Parse(bu)
			if err != nil {
				return fmt.Errorf("weixin base_url invalid: %w", err)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("weixin base_url must use http or https (got %q)", u.Scheme)
			}
			if u.Host == "" {
				return fmt.Errorf("weixin base_url must have a host")
			}
			// Literal-IP guard only; DNS-based SSRF needs a runtime Dialer hook
			// and the redirect variant is blocked by CheckRedirect elsewhere.
			if host := u.Hostname(); host != "" {
				if ip := net.ParseIP(host); ip != nil {
					if ip.IsLoopback() || ip.IsPrivate() ||
						ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
						ip.IsUnspecified() {
						return fmt.Errorf("weixin base_url host %q is a loopback/private/link-local address; refusing (SSRF guard)", host)
					}
				}
			}
		}
	}
	return nil
}
