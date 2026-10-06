package config

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/imbudget"
)

func TestValidateIMLimits(t *testing.T) {
	t.Parallel()
	f := false
	cases := []struct {
		name    string
		limits  IMLimitsConfig
		costOff bool
		wantErr string
	}{
		{name: "absent is fine"},
		{name: "budget only", limits: IMLimitsConfig{PerChatDailyUSD: 20}},
		{name: "rate only", limits: IMLimitsConfig{UserRate: IMUserRate{PerMinute: 10}}},
		{name: "negative budget", limits: IMLimitsConfig{PerChatDailyUSD: -1}, wantErr: "negative"},
		{name: "warn ratio 1", limits: IMLimitsConfig{PerChatDailyUSD: 1, WarnRatio: 1}, wantErr: "warn ratio"},
		{name: "bad action", limits: IMLimitsConfig{PerChatDailyUSD: 1, Action: "soft"}, wantErr: "action"},
		{name: "budget with ledger off", limits: IMLimitsConfig{PerChatDailyUSD: 1}, costOff: true, wantErr: "cost.enabled"},
		{name: "rate with ledger off is fine", limits: IMLimitsConfig{UserRate: IMUserRate{PerMinute: 1}}, costOff: true},
		{name: "negative per_minute", limits: IMLimitsConfig{UserRate: IMUserRate{PerMinute: -1}}, wantErr: "per_minute"},
		{name: "burst without rate", limits: IMLimitsConfig{UserRate: IMUserRate{Burst: 3}}, wantErr: "per_minute is 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{IMLimits: tc.limits}
			if tc.costOff {
				cfg.Cost.Enabled = &f
			}
			err := validateIMLimits(cfg)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestIMLimits_PolicyAndBurst(t *testing.T) {
	t.Parallel()
	l := IMLimitsConfig{PerChatDailyUSD: 5, Action: "warn", UserRate: IMUserRate{PerMinute: 6}}
	p := l.BudgetPolicy()
	if p.PerChatUSD != 5 || p.Action != imbudget.ActionWarn || p.Window.Hours() != 24 {
		t.Fatalf("policy = %+v", p)
	}
	if l.EffectiveBurst() != 6 {
		t.Fatalf("burst defaults to per_minute, got %d", l.EffectiveBurst())
	}
	l.UserRate.Burst = 2
	if l.EffectiveBurst() != 2 {
		t.Fatalf("explicit burst, got %d", l.EffectiveBurst())
	}
}

func TestLoad_IMLimitsFromYAML(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeCfg(t, `im_limits:
  per_chat_daily_usd: 12.5
  warn_ratio: 0.5
  action: warn
  user_rate:
    per_minute: 8
    burst: 3
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMLimits.PerChatDailyUSD != 12.5 || cfg.IMLimits.WarnRatio != 0.5 || cfg.IMLimits.Action != "warn" ||
		cfg.IMLimits.UserRate.PerMinute != 8 || cfg.IMLimits.UserRate.Burst != 3 {
		t.Fatalf("im_limits = %+v", cfg.IMLimits)
	}
}
