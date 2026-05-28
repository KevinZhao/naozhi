package cron

import "testing"

// TestResolveNotifyDecision_SourcesAllBranches asserts each priority-ladder
// branch returns its dedicated NotifySource enum so dashboard / debug
// surfaces can explain *why* a particular target was selected.
// R241-ARCH-12 (#520).
func TestResolveNotifyDecision_SourcesAllBranches(t *testing.T) {
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
		wantSet    bool
	}{
		{
			name:    "explicit_disable_wins",
			notify:  boolPtr(false),
			wantSrc: NotifySourceExplicitDisable,
			wantSet: false,
		},
		{
			name:       "per_job_override",
			platName:   "dashboard",
			chatID:     "global",
			notifyPlat: "feishu",
			notifyChat: "oc_x",
			wantSrc:    NotifySourcePerJobOverride,
			wantSet:    true,
		},
		{
			name:      "default_when_enabled",
			notify:    boolPtr(true),
			notifyDef: NotifyTarget{Platform: "feishu", ChatID: "oc_default"},
			wantSrc:   NotifySourceDefault,
			wantSet:   true,
		},
		{
			name:    "default_missing_when_enabled",
			notify:  boolPtr(true),
			wantSrc: NotifySourceDefaultMissing,
			wantSet: false,
		},
		{
			name:     "legacy_source_chat",
			platName: "feishu",
			chatID:   "oc_y",
			wantSrc:  NotifySourceLegacySourceChat,
			wantSet:  true,
		},
		{
			name:     "dashboard_silent",
			platName: "dashboard",
			chatID:   "global",
			wantSrc:  NotifySourceDashboardSilent,
			wantSet:  false,
		},
		{
			name:    "none_when_empty",
			wantSrc: NotifySourceNone,
			wantSet: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scheduler{notifyDefault: tc.notifyDef}
			got := s.resolveNotifyDecision(tc.platName, tc.chatID, tc.notifyPlat, tc.notifyChat, tc.notify)
			if got.Source != tc.wantSrc {
				t.Errorf("Source = %v (%s), want %v (%s)", got.Source, got.Source, tc.wantSrc, tc.wantSrc)
			}
			if got.Target.IsSet() != tc.wantSet {
				t.Errorf("Target.IsSet() = %v, want %v (target=%+v)", got.Target.IsSet(), tc.wantSet, got.Target)
			}
		})
	}
}

// TestNotifySource_StringStable pins the lower_snake string contract so
// dashboards / log greppers can match on the value rather than the
// numeric ordinal. Stable across versions; reordering or renaming is a
// breaking change and must come with a release-note callout.
func TestNotifySource_StringStable(t *testing.T) {
	t.Parallel()
	cases := map[NotifySource]string{
		NotifySourceNone:             "none",
		NotifySourceExplicitDisable:  "explicit_disable",
		NotifySourcePerJobOverride:   "per_job_override",
		NotifySourceDefault:          "default",
		NotifySourceDefaultMissing:   "default_missing",
		NotifySourceLegacySourceChat: "legacy_source_chat",
		NotifySourceDashboardSilent:  "dashboard_silent",
	}
	for src, want := range cases {
		if got := src.String(); got != want {
			t.Errorf("NotifySource(%d).String() = %q, want %q", int(src), got, want)
		}
	}
	// Unknown ordinals fall back to "none" so a future renumber doesn't
	// crash log emitters.
	if got := NotifySource(99).String(); got != "none" {
		t.Errorf("unknown ordinal should fall back to \"none\", got %q", got)
	}
}

// TestResolveNotifyTarget_DelegatesToDecision is a smoke test that the
// public resolveNotifyTarget wrapper still returns the same target as
// resolveNotifyDecision for every branch, so the refactor is purely
// additive (no caller-visible drift).
func TestResolveNotifyTarget_DelegatesToDecision(t *testing.T) {
	t.Parallel()
	s := &Scheduler{notifyDefault: NotifyTarget{Platform: "feishu", ChatID: "oc_default"}}
	scenarios := []struct {
		name string
		args [5]any // platName, chatID, notifyPlat, notifyChat, notify
	}{
		{"explicit_disable", [5]any{"feishu", "oc", "", "", boolPtr(false)}},
		{"per_job_override", [5]any{"dashboard", "global", "feishu", "oc_x", (*bool)(nil)}},
		{"default", [5]any{"", "", "", "", boolPtr(true)}},
		{"legacy_source_chat", [5]any{"feishu", "oc_y", "", "", (*bool)(nil)}},
		{"dashboard_silent", [5]any{"dashboard", "global", "", "", (*bool)(nil)}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			pn := sc.args[0].(string)
			ch := sc.args[1].(string)
			np := sc.args[2].(string)
			nc := sc.args[3].(string)
			n := sc.args[4].(*bool)
			gotTarget := s.resolveNotifyTarget(pn, ch, np, nc, n)
			gotDecision := s.resolveNotifyDecision(pn, ch, np, nc, n)
			if gotTarget != gotDecision.Target {
				t.Errorf("resolveNotifyTarget != resolveNotifyDecision.Target: %+v vs %+v",
					gotTarget, gotDecision.Target)
			}
		})
	}
}
