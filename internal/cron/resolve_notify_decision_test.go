package cron

import "testing"

func boolPtr(b bool) *bool { return &b }

// TestResolveNotifyDecision walks the priority ladder: each branch reports its
// NotifySource and the target it picked. Dashboard jobs persist with
// platform="dashboard", which is never a registered IM platform, so the legacy
// branch must stay silent for them rather than warn "platform not found" on
// every tick; a per-job override still wins over that, and notify=false wins
// over everything.
func TestResolveNotifyDecision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		platName   string
		chatID     string
		notifyPlat string
		notifyChat string
		notify     *bool
		notifyDef  NotifyTarget
		wantSrc    NotifySource
		want       NotifyTarget
	}{
		{name: "explicit_disable_wins", notify: boolPtr(false), wantSrc: NotifySourceExplicitDisable},
		{
			name: "explicit_disable_beats_override", platName: "dashboard", chatID: "global",
			notifyPlat: "feishu", notifyChat: "oc_x", notify: boolPtr(false),
			wantSrc: NotifySourceExplicitDisable,
		},
		{
			name: "per_job_override", platName: "dashboard", chatID: "global",
			notifyPlat: "feishu", notifyChat: "oc_x",
			wantSrc: NotifySourcePerJobOverride, want: NotifyTarget{Platform: "feishu", ChatID: "oc_x"},
		},
		{
			name: "default_when_enabled", notify: boolPtr(true),
			notifyDef: NotifyTarget{Platform: "feishu", ChatID: "oc_default"},
			wantSrc:   NotifySourceDefault, want: NotifyTarget{Platform: "feishu", ChatID: "oc_default"},
		},
		{name: "default_missing_when_enabled", notify: boolPtr(true), wantSrc: NotifySourceDefaultMissing},
		{
			name: "legacy_source_chat", platName: "feishu", chatID: "oc_y",
			wantSrc: NotifySourceLegacySourceChat, want: NotifyTarget{Platform: "feishu", ChatID: "oc_y"},
		},
		{name: "dashboard_silent", platName: "dashboard", chatID: "global", wantSrc: NotifySourceDashboardSilent},
		{name: "none_when_empty", wantSrc: NotifySourceNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scheduler{notifyDefault: tc.notifyDef}
			got := s.resolveNotifyDecision(tc.platName, tc.chatID, tc.notifyPlat, tc.notifyChat, tc.notify)
			if got.Source != tc.wantSrc {
				t.Errorf("Source = %v (%s), want %v (%s)", got.Source, got.Source, tc.wantSrc, tc.wantSrc)
			}
			if got.Target != tc.want {
				t.Errorf("Target = %+v, want %+v", got.Target, tc.want)
			}
		})
	}
}
