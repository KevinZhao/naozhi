package config

import (
	"fmt"
	"time"

	"github.com/naozhi/naozhi/internal/imbudget"
)

// IMLimitsConfig is the im_limits block: how much an IM chat may spend and
// how fast a sender may message. Absent keeps both gates off. See
// docs/rfc/im-usage-limits.md.
type IMLimitsConfig struct {
	// PerChatDailyUSD is the rolling-24h USD budget per chat; 0 disables.
	PerChatDailyUSD float64 `yaml:"per_chat_daily_usd,omitempty"`
	// WarnRatio in (0,1) is where the once-per-window warning fires; 0 = 0.8.
	WarnRatio float64 `yaml:"warn_ratio,omitempty"`
	// Action is "block" (default) or "warn".
	Action string `yaml:"action,omitempty"`
	// UserRate limits messages per sender.
	UserRate IMUserRate `yaml:"user_rate,omitempty"`
}

// IMUserRate is a per-sender token bucket; PerMinute 0 disables it.
type IMUserRate struct {
	PerMinute int `yaml:"per_minute,omitempty"`
	// Burst defaults to PerMinute.
	Burst int `yaml:"burst,omitempty"`
}

// BudgetEnabled reports whether a per-chat budget is configured.
func (c IMLimitsConfig) BudgetEnabled() bool { return c.PerChatDailyUSD > 0 }

// RateEnabled reports whether a per-sender rate limit is configured.
func (c IMLimitsConfig) RateEnabled() bool { return c.UserRate.PerMinute > 0 }

// BudgetPolicy converts the block into the gate's policy.
func (c IMLimitsConfig) BudgetPolicy() imbudget.Policy {
	return imbudget.Policy{
		PerChatUSD: c.PerChatDailyUSD,
		WarnRatio:  c.WarnRatio,
		Action:     imbudget.Action(c.Action),
		Window:     24 * time.Hour,
	}
}

// EffectiveBurst is UserRate.Burst, or PerMinute when unset.
func (c IMLimitsConfig) EffectiveBurst() int {
	if c.UserRate.Burst > 0 {
		return c.UserRate.Burst
	}
	return c.UserRate.PerMinute
}

// validateIMLimits rejects values the gates cannot honour, and a budget on a
// deployment whose cost ledger is off (the gate would read 0 forever).
func validateIMLimits(cfg *Config) error {
	l := cfg.IMLimits
	if err := l.BudgetPolicy().Validate(); err != nil {
		return fmt.Errorf("im_limits: %w", err)
	}
	if l.BudgetEnabled() && !cfg.Cost.IsEnabled() {
		return fmt.Errorf("im_limits.per_chat_daily_usd is set but cost.enabled is false; the budget gate reads the cost ledger")
	}
	switch {
	case l.UserRate.PerMinute < 0:
		return fmt.Errorf("im_limits.user_rate.per_minute is negative")
	case l.UserRate.Burst < 0:
		return fmt.Errorf("im_limits.user_rate.burst is negative")
	case l.UserRate.Burst > 0 && l.UserRate.PerMinute == 0:
		return fmt.Errorf("im_limits.user_rate.burst is set but per_minute is 0; set per_minute to enable the limit")
	}
	return nil
}
