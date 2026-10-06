package config

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// CostConfig tunes the cost ledger (docs/rfc/cost-ledger.md §9). Enabled
// defaults to true; the ledger lives beside session.store_path, so it is also
// off when that is empty. Out-of-range day counts are clamped by the ledger.
type CostConfig struct {
	Enabled       *bool            `yaml:"enabled,omitempty"`
	RetentionDays int              `yaml:"retention_days,omitempty"`
	RollupDays    int              `yaml:"rollup_days,omitempty"`
	Budget        CostBudgetConfig `yaml:"budget,omitempty"`
}

// IsEnabled resolves the tri-state Enabled flag (nil = true).
func (c CostConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// CostBudgetConfig is cost.budget: daily USD caps per cron job and for the
// whole machine, counted from the cost ledger (internal/budget), 0 = off.
// Spend metered in credits or tokens is not counted. The day starts at
// midnight in Timezone, else cron.timezone. WarnRatio 0 means 0.8; Action ""
// means block.
type CostBudgetConfig struct {
	PerCronJobDailyUSD float64 `yaml:"per_cron_job_daily_usd,omitempty"`
	DailyUSD           float64 `yaml:"daily_usd,omitempty"`
	WarnRatio          float64 `yaml:"warn_ratio,omitempty"`
	Action             string  `yaml:"action,omitempty"`
	Timezone           string  `yaml:"timezone,omitempty"`
}

// HasLimit reports whether any cap is set.
func (b CostBudgetConfig) HasLimit() bool {
	return b.PerCronJobDailyUSD > 0 || b.DailyUSD > 0
}

// BudgetLocation is where a cost.budget day starts: cost.budget.timezone,
// else the cron time zone.
func (c *Config) BudgetLocation() *time.Location {
	name := strings.TrimSpace(c.Cost.Budget.Timezone)
	if name == "" {
		return c.ParseCronTimezone()
	}
	if strings.EqualFold(name, "Local") {
		return time.Local
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.Local
}

// validateCostBudget refuses a cost.budget that would not do what it says: a
// negative or non-finite cap, a warn_ratio outside (0,1], an unknown action
// or time zone, settings with no cap, and caps with the ledger off (spend
// would read 0 forever).
func validateCostBudget(cfg *Config) error {
	b := cfg.Cost.Budget
	for _, f := range []struct {
		key string
		v   float64
	}{{"per_cron_job_daily_usd", b.PerCronJobDailyUSD}, {"daily_usd", b.DailyUSD}} {
		if f.v < 0 || math.IsNaN(f.v) || math.IsInf(f.v, 0) {
			return fmt.Errorf("cost.budget.%s must be a USD amount >= 0 (0 = off), got %v", f.key, f.v)
		}
	}
	if b.WarnRatio < 0 || b.WarnRatio > 1 || math.IsNaN(b.WarnRatio) {
		return fmt.Errorf("cost.budget.warn_ratio must be in (0, 1] (omit for 0.8), got %v", b.WarnRatio)
	}
	switch b.Action {
	case "", "block", "warn":
	default:
		return fmt.Errorf("cost.budget.action must be block or warn, got %q", b.Action)
	}
	if name := strings.TrimSpace(b.Timezone); name != "" && !strings.EqualFold(name, "Local") {
		if _, err := time.LoadLocation(name); err != nil {
			return fmt.Errorf("cost.budget.timezone %q: %v", b.Timezone, err)
		}
	}
	if !b.HasLimit() {
		if b != (CostBudgetConfig{}) {
			return fmt.Errorf("cost.budget sets no per_cron_job_daily_usd or daily_usd; set a cap, or remove the block")
		}
		return nil
	}
	if !cfg.Cost.IsEnabled() {
		return fmt.Errorf("cost.budget needs the cost ledger; remove cost.enabled: false, or remove cost.budget")
	}
	return nil
}
