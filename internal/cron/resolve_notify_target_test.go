package cron

import "testing"

func boolPtr(b bool) *bool { return &b }

// TestResolveNotifyTarget_DashboardLegacyNoop pins the dashboard short-circuit
// in the legacy (notify==nil) branch. Dashboard-created jobs persist with
// platform="dashboard"/chat_id="global"; "dashboard" is never a registered
// IM platform (cmd/naozhi/main.go only registers feishu/slack/discord/weixin),
// so without this guard every dashboard tick fires
// "cron notify: platform not found" via notifyTarget. Observed in production
// logs as a per-tick WARN that ran for weeks before the guard landed.
func TestResolveNotifyTarget_DashboardLegacyNoop(t *testing.T) {
	t.Parallel()
	s := &Scheduler{}
	got := s.resolveNotifyTarget("dashboard", "global", "", "", nil)
	if got.IsSet() {
		t.Fatalf("dashboard legacy job must resolve to empty target; got %+v", got)
	}
}

// TestResolveNotifyTarget_DashboardWithPerJobOverride confirms the dashboard
// short-circuit only kicks in on the legacy fall-through. When a user fills
// notify_platform/notify_chat_id on the dashboard form, that override must
// still win — the legacy guard runs strictly after the per-job branch.
func TestResolveNotifyTarget_DashboardWithPerJobOverride(t *testing.T) {
	t.Parallel()
	s := &Scheduler{}
	got := s.resolveNotifyTarget("dashboard", "global", "feishu", "oc_x", nil)
	if got.Platform != "feishu" || got.ChatID != "oc_x" {
		t.Fatalf("per-job override must beat dashboard short-circuit; got %+v", got)
	}
}

// TestResolveNotifyTarget_IMLegacyStillReplies guards against an over-broad
// rewrite: legacy (notify==nil) IM-created jobs must still reply to their
// source chat. Only platName=="dashboard" should no-op.
func TestResolveNotifyTarget_IMLegacyStillReplies(t *testing.T) {
	t.Parallel()
	s := &Scheduler{}
	got := s.resolveNotifyTarget("feishu", "oc_x", "", "", nil)
	if got.Platform != "feishu" || got.ChatID != "oc_x" {
		t.Fatalf("legacy IM job must reply to source chat; got %+v", got)
	}
}

// TestResolveNotifyTarget_DashboardExplicitDisable keeps the explicit-disable
// precedence above the dashboard guard: notify=false short-circuits everything.
func TestResolveNotifyTarget_DashboardExplicitDisable(t *testing.T) {
	t.Parallel()
	s := &Scheduler{}
	got := s.resolveNotifyTarget("dashboard", "global", "feishu", "oc_x", boolPtr(false))
	if got.IsSet() {
		t.Fatalf("notify=false must win even with per-job override; got %+v", got)
	}
}
