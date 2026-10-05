package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// /cron mode <id> fresh|keep reaches the scheduler with the chat's scope and
// the parsed mode, and anything else is answered with the usage line without
// calling it. The reply names the resulting mode and, for a paused job, how to
// resume it.
func TestHandleCronMode(t *testing.T) {
	const usage = "用法: /cron mode <id> fresh|keep"
	cases := []struct {
		name      string
		text      string
		paused    bool
		wantCall  bool
		wantFresh bool
		want      string
		notWant   string
	}{
		{name: "no args", text: "/cron mode", want: usage},
		{name: "id only", text: "/cron mode abcd", want: usage},
		{name: "extra token", text: "/cron mode abcd fresh now", want: usage},
		{name: "bad mode", text: "/cron mode abcd new", want: usage},
		{name: "oversized id", text: "/cron mode " + strings.Repeat("a", maxCronIDLen+1) + " fresh", want: "无效 ID"},
		{name: "fresh", text: "/cron mode abcd fresh", wantCall: true, wantFresh: true,
			want: "Job abcd 已改为每次执行都从新会话开始（下次执行生效）。", notWant: "/cron resume"},
		{name: "keep", text: "/cron mode abcd keep", wantCall: true,
			want: "Job abcd 已改为每次执行延续同一会话的上下文（下次执行生效）。", notWant: "/cron resume"},
		{name: "upper case", text: "/cron mode abcd KEEP", wantCall: true, want: "延续同一会话"},
		{name: "keep-context alias", text: "/cron mode abcd Keep-Context", wantCall: true, want: "延续同一会话"},
		{name: "ideographic space", text: "/cron mode abcd　fresh", wantCall: true, wantFresh: true, want: "从新会话开始"},
		{name: "upper sub-command", text: "/cron MODE abcd fresh", wantCall: true, wantFresh: true, want: "从新会话开始"},
		{name: "paused", text: "/cron mode abcd fresh", paused: true, wantCall: true, wantFresh: true,
			want: "\n该任务当前已暂停，发送 /cron resume abcd 恢复。"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fp := &fakePlatform{}
			d := newTestDispatcher(fp)
			fake := &fakeCronScheduler{setFreshPaused: c.paused}
			d.scheduler = fake
			d.handleCronCommand(context.Background(), incomingMsg(c.text), c.text, slog.Default())
			got := fp.lastReply()
			if !strings.Contains(got, c.want) {
				t.Errorf("reply = %q, want substring %q", got, c.want)
			}
			if c.notWant != "" && strings.Contains(got, c.notWant) {
				t.Errorf("reply = %q must not contain %q", got, c.notWant)
			}
			if !c.wantCall {
				if fake.setFreshCalls != 0 {
					t.Errorf("SetFreshContext called %d times for a malformed command", fake.setFreshCalls)
				}
				return
			}
			if fake.setFreshCalls != 1 {
				t.Fatalf("SetFreshContext calls = %d, want 1", fake.setFreshCalls)
			}
			call := fake.lastSetFresh
			if call.idPrefix != "abcd" || call.plat != "fake" || call.chatID != "chat1" || call.fresh != c.wantFresh {
				t.Errorf("SetFreshContext(%q, %q, %q, %v), want (abcd, fake, chat1, %v)",
					call.idPrefix, call.plat, call.chatID, call.fresh, c.wantFresh)
			}
		})
	}
}

// A refused mode change is answered by error class, never with the raw error.
func TestHandleCronMode_ErrorReplyByClass(t *testing.T) {
	cases := []struct{ code, want string }{
		{CronCodeJobNotFound, "修改失败：未找到该 ID 对应的任务"},
		{CronCodeAmbiguousPrefix, "ID 前缀匹配到多个任务"},
		{"", "修改失败：请确认 ID 正确。"},
	}
	for _, c := range cases {
		fp := &fakePlatform{}
		d := newTestDispatcher(fp)
		d.scheduler = &fakeCronScheduler{setFreshErr: errors.New("cron: secret internals"), classifyResult: c.code}
		d.handleCronCommand(context.Background(), incomingMsg("/cron mode ab fresh"), "/cron mode ab fresh", slog.Default())
		got := fp.lastReply()
		if !strings.Contains(got, c.want) || strings.Contains(got, "secret") {
			t.Errorf("code %q: reply = %q, want substring %q and no raw error", c.code, got, c.want)
		}
	}
}

// The /cron usage block and /help both advertise the mode command.
func TestCronMode_ListedInUsageAndHelp(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.scheduler = &fakeCronScheduler{}
	d.handleCronCommand(context.Background(), incomingMsg("/cron"), "/cron", slog.Default())
	if got := fp.lastReply(); !strings.Contains(got, "/cron <add|list|del|pause|resume|mode>") ||
		!strings.Contains(got, "/cron mode <id> fresh|keep") {
		t.Errorf("/cron usage = %q, want the mode subcommand listed", got)
	}
	d.handleHelpCommand(context.Background(), incomingMsg("/help"))
	if got := fp.lastReply(); !strings.Contains(got, "/cron <add|list|del|pause|resume|mode>") {
		t.Errorf("/help = %q, want the mode subcommand listed", got)
	}
}
