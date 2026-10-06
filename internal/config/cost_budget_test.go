package config

import (
	"strings"
	"testing"
	"time"
)

// Each cost.budget that would not do what it says is fatal at load.
func TestLoad_CostBudgetRefusals(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"negative cap", "cost:\n  budget:\n    per_chat_daily_usd: -1\n", "cost.budget.per_chat_daily_usd must be a USD amount >= 0"},
		{"infinite cap", "cost:\n  budget:\n    daily_usd: .inf\n", "cost.budget.daily_usd must be a USD amount >= 0"},
		{"nan cap", "cost:\n  budget:\n    per_cron_job_daily_usd: .nan\n", "cost.budget.per_cron_job_daily_usd must be a USD amount >= 0"},
		{"warn ratio above one", "cost:\n  budget:\n    daily_usd: 5\n    warn_ratio: 1.5\n", "cost.budget.warn_ratio must be in (0, 1]"},
		{"negative warn ratio", "cost:\n  budget:\n    daily_usd: 5\n    warn_ratio: -0.1\n", "cost.budget.warn_ratio must be in (0, 1]"},
		{"unknown action", "cost:\n  budget:\n    daily_usd: 5\n    action: deny\n", `cost.budget.action must be block or warn, got "deny"`},
		{"unknown zone", "cost:\n  budget:\n    daily_usd: 5\n    timezone: Mars/Olympus\n", `cost.budget.timezone "Mars/Olympus"`},
		{"settings without a cap", "cost:\n  budget:\n    action: warn\n", "cost.budget sets no per_chat_daily_usd"},
		{"ledger off", "cost:\n  enabled: false\n  budget:\n    per_chat_daily_usd: 5\n", "cost.budget needs the cost ledger"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			if err == nil {
				t.Fatalf("Load accepted:\n%s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoad_CostBudgetAccepted(t *testing.T) {
	cfg, err := Load(writeCfg(t, `cost:
  budget:
    per_chat_daily_usd: 20
    per_cron_job_daily_usd: 5
    daily_usd: 100
    warn_ratio: 1
    action: warn
    timezone: UTC
`))
	if err != nil {
		t.Fatal(err)
	}
	want := CostBudgetConfig{PerChatDailyUSD: 20, PerCronJobDailyUSD: 5, DailyUSD: 100, WarnRatio: 1, Action: "warn", Timezone: "UTC"}
	if got := cfg.Cost.Budget; got != want || !got.HasLimit() {
		t.Errorf("budget = %+v, want %+v", got, want)
	}
	if loc := cfg.BudgetLocation(); loc != time.UTC {
		t.Errorf("BudgetLocation = %v, want UTC", loc)
	}
	if _, err := Load(writeCfg(t, "cost:\n  enabled: false\n")); err != nil {
		t.Errorf("no budget with the ledger off must load: %v", err)
	}
}

// With no cost.budget.timezone the day follows cron.timezone.
func TestBudgetLocation_FallsBackToCronZone(t *testing.T) {
	cfg := &Config{}
	cfg.Cron.Timezone = "Asia/Shanghai"
	if got := cfg.BudgetLocation().String(); got != "Asia/Shanghai" {
		t.Errorf("BudgetLocation = %s, want the cron zone", got)
	}
	cfg.Cost.Budget.Timezone = "Local"
	if cfg.BudgetLocation() != time.Local {
		t.Error("timezone Local must mean time.Local")
	}
}
