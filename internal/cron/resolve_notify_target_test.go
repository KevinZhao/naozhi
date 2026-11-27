package cron

import "testing"

func boolPtr(b bool) *bool { return &b }

// TestResolveNotifyTarget walks the priority ladder. Dashboard jobs persist with
// platform="dashboard", which is never a registered IM platform, so the legacy
// branch must stay silent for them rather than warn "platform not found" on
// every tick; a per-job override still wins over that, and notify=false wins
// over everything.
func TestResolveNotifyTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		platName   string
		chatID     string
		notifyPlat string
		notifyChat string
		notify     *bool
		notifyDef  NotifyTarget
		want       NotifyTarget
	}{
		{name: "explicit_disable_wins", notify: boolPtr(false)},
		{
			name: "explicit_disable_beats_override", platName: "dashboard", chatID: "global",
			notifyPlat: "feishu", notifyChat: "oc_x", notify: boolPtr(false),
		},
		{
			name: "per_job_override", platName: "dashboard", chatID: "global",
			notifyPlat: "feishu", notifyChat: "oc_x", want: NotifyTarget{Platform: "feishu", ChatID: "oc_x"},
		},
		{
			name: "default_when_enabled", notify: boolPtr(true),
			notifyDef: NotifyTarget{Platform: "feishu", ChatID: "oc_default"}, want: NotifyTarget{Platform: "feishu", ChatID: "oc_default"},
		},
		{name: "default_missing_when_enabled", notify: boolPtr(true)},
		{
			name: "legacy_source_chat", platName: "feishu", chatID: "oc_y", want: NotifyTarget{Platform: "feishu", ChatID: "oc_y"},
		},
		{name: "dashboard_silent", platName: "dashboard", chatID: "global"},
		{name: "none_when_empty"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scheduler{notifyDefault: tc.notifyDef}
			if got := s.resolveNotifyTarget(tc.platName, tc.chatID, tc.notifyPlat, tc.notifyChat, tc.notify); got != tc.want {
				t.Errorf("target = %+v, want %+v", got, tc.want)
			}
		})
	}
}
