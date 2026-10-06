package dispatch

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

func runCmd(t *testing.T, d *Dispatcher, fp *fakePlatform, text string) string {
	t.Helper()
	if !d.dispatchCommand(context.Background(), incomingMsg(text), text, slog.Default()) {
		t.Fatalf("%q was not recognised as a command", text)
	}
	return fp.lastReply()
}

// tuningFixture is a dispatcher over a router holding one detached (no
// process) session for the fake chat's general agent, so SetSessionTuning
// records on a real ManagedSession and the snapshot reports it back.
func tuningFixture(t *testing.T, agents map[string]string) (*Dispatcher, *fakePlatform, *session.Router, string) {
	t.Helper()
	fp := &fakePlatform{}
	router := session.NewRouter(session.RouterConfig{MaxProcs: 10})
	d := newTestDispatcher(fp, withRouter(router), func(c *testDispatcherConfig) {
		if agents != nil {
			c.AgentCommands = agents
		}
	})
	msg := incomingMsg("")
	key := d.keyForChat(msg.Platform, msg.ChatType, msg.ChatID, "general")
	router.InjectSession(key, nil)
	return d, fp, router, key
}

func TestModelCommand_ShowSetReset(t *testing.T) {
	t.Parallel()
	d, fp, _, key := tuningFixture(t, nil)

	if got := runCmd(t, d, fp, "/model"); !strings.Contains(got, "model: （配置默认）") || !strings.Contains(got, "用法") {
		t.Fatalf("empty /model: %q", got)
	}
	// Detached session: no live process, so the override is recorded for
	// the next spawn (deferred).
	if got := runCmd(t, d, fp, "/model opus"); !strings.Contains(got, "已记录为 opus") {
		t.Fatalf("/model opus: %q", got)
	}
	snap, ok := d.snapshotForKey(key)
	if !ok || snap.TuningModel != "opus" {
		t.Fatalf("snapshot = %+v ok=%v, want TuningModel opus", snap, ok)
	}
	if got := runCmd(t, d, fp, "/model"); !strings.Contains(got, "model: opus（会话覆盖）") {
		t.Fatalf("/model after set: %q", got)
	}
	if got := runCmd(t, d, fp, "/model reset"); !strings.Contains(got, "配置默认") {
		t.Fatalf("/model reset: %q", got)
	}
	if snap, _ := d.snapshotForKey(key); snap.TuningModel != "" {
		t.Fatalf("reset left TuningModel=%q", snap.TuningModel)
	}
}

func TestModelCommand_NoSessionYet(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	if got := runCmd(t, d, fp, "/model"); !strings.Contains(got, "尚未创建") {
		t.Fatalf("fresh chat: %q", got)
	}
	// The pick is parked in the router and applies on the first spawn.
	if got := runCmd(t, d, fp, "/model opus"); !strings.Contains(got, "会话启动时生效") {
		t.Fatalf("/model opus on fresh chat: %q", got)
	}
}

func TestModelCommand_RejectsFlagShapedName(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	if got := runCmd(t, d, fp, "/model --dangerously-skip-permissions"); !strings.Contains(got, "无效的模型名") {
		t.Fatalf("flag-shaped model accepted: %q", got)
	}
	if got := runCmd(t, d, fp, "/model opus review extra"); !strings.Contains(got, "参数过多") {
		t.Fatalf("too many args: %q", got)
	}
}

func TestEffortCommand_ValidatesTier(t *testing.T) {
	t.Parallel()
	d, fp, _, key := tuningFixture(t, nil)
	if got := runCmd(t, d, fp, "/effort turbo"); !strings.Contains(got, "无效的 effort") || !strings.Contains(got, "high") {
		t.Fatalf("bad tier: %q", got)
	}
	// A detached session has no wrapper to confirm the capability; the
	// router refuses rather than record a tier that may never apply.
	if got := runCmd(t, d, fp, "/effort HIGH"); !strings.Contains(got, "不支持 effort") {
		t.Fatalf("effort on capability-less session: %q", got)
	}
	if snap, _ := d.snapshotForKey(key); snap.TuningEffort != "" {
		t.Fatalf("refused effort was recorded: %q", snap.TuningEffort)
	}
}

func TestTuningCommands_AgentToken(t *testing.T) {
	t.Parallel()
	d, fp, router, generalKey := tuningFixture(t, map[string]string{"review": "code-reviewer"})
	msg := incomingMsg("")
	reviewKey := d.keyForChat(msg.Platform, msg.ChatType, msg.ChatID, "code-reviewer")
	router.InjectSession(reviewKey, nil)

	if got := runCmd(t, d, fp, "/model sonnet nope"); !strings.Contains(got, "未知的 agent") {
		t.Fatalf("unknown agent: %q", got)
	}
	runCmd(t, d, fp, "/model sonnet review")
	if snap, _ := d.snapshotForKey(generalKey); snap.TuningModel != "" {
		t.Fatal("agent-scoped /model touched the general key")
	}
	if snap, _ := d.snapshotForKey(reviewKey); snap.TuningModel != "sonnet" {
		t.Fatalf("code-reviewer TuningModel = %q, want sonnet", snap.TuningModel)
	}
}

func TestBackendCommand_ShowSetUnknownReset(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, func(c *testDispatcherConfig) {
		c.Capabilities = fakeCapabilities{
			takeover:   func(context.Context, string, string, session.AgentOpts) bool { return false },
			backendIDs: []string{"claude", "kiro"},
		}
	})
	got := runCmd(t, d, fp, "/backend")
	if !strings.Contains(got, "当前 backend: claude") || !strings.Contains(got, "可用: claude, kiro") {
		t.Fatalf("/backend show: %q", got)
	}
	if got := runCmd(t, d, fp, "/backend codex"); !strings.Contains(got, "未知的 backend") {
		t.Fatalf("/backend codex: %q", got)
	}
	if got := runCmd(t, d, fp, "/backend kiro"); !strings.Contains(got, "backend 已设为 kiro") || !strings.Contains(got, "会话启动时生效") {
		t.Fatalf("/backend kiro on fresh chat: %q", got)
	}
	if got := runCmd(t, d, fp, "/backend reset"); !strings.Contains(got, "配置默认") {
		t.Fatalf("/backend reset: %q", got)
	}
}

func TestBackendCommand_LiveSessionNeedsNew(t *testing.T) {
	t.Parallel()
	d, fp, _, _ := tuningFixture(t, nil)
	d.caps = fakeCapabilities{backendIDs: []string{"claude", "kiro"}}
	got := runCmd(t, d, fp, "/backend kiro")
	if !strings.Contains(got, "发送 /new 后生效") {
		t.Fatalf("/backend on an existing session: %q", got)
	}
}

func TestHelpCommand_ListsTuningCommands(t *testing.T) {
	t.Parallel()
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	got := runCmd(t, d, fp, "/help")
	for _, want := range []string{"/model", "/effort", "/backend"} {
		if !strings.Contains(got, want) {
			t.Errorf("/help lacks %s: %q", want, got)
		}
	}
}

func TestAppliedText_Modes(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		sessionview.TuningAppliedRPC:      "立即生效",
		sessionview.TuningAppliedRespawn:  "下一条消息起生效",
		sessionview.TuningAppliedDeferred: "会话启动时生效",
	}
	for via, want := range cases {
		if got := appliedText("模型", "opus", via); !strings.Contains(got, want) {
			t.Errorf("%s: %q lacks %q", via, got, want)
		}
	}
}
