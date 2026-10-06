package dispatch

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/imauth"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/turn"
)

// countingTurns counts what reaches the orchestrator; a refused message must
// add nothing.
type countingTurns struct {
	Turns
	submits atomic.Int64
	resets  atomic.Int64
}

func (c *countingTurns) Submit(ctx context.Context, r turn.Request, a turn.Admission) turn.Ack {
	c.submits.Add(1)
	return c.Turns.Submit(ctx, r, a)
}

func (c *countingTurns) Reset(ctx context.Context, key string, discardOverride bool) {
	c.resets.Add(1)
	c.Turns.Reset(ctx, key, discardOverride)
}

func idSet(ids ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

// aliceAndRoot lets alice chat and makes root the only admin on "fake".
func aliceAndRoot() *imauth.Policy {
	return &imauth.Policy{Rules: map[string]imauth.Rule{
		"fake": {Allowed: idSet("alice"), Admins: idSet("root")},
	}}
}

func newAuthzDispatcher(t *testing.T, p *imauth.Policy) (*Dispatcher, *fakePlatform, *countingTurns, *fakeCronScheduler) {
	t.Helper()
	fp := &fakePlatform{}
	cron := &fakeCronScheduler{}
	d := newTestDispatcher(fp, func(cfg *testDispatcherConfig) {
		cfg.Access = p
		cfg.Scheduler = cron
	})
	ct := &countingTurns{Turns: d.turns}
	d.turns = ct
	return d, fp, ct, cron
}

var authzEvent atomic.Int64

func authzMsg(user, chatType, text string) platform.IncomingMessage {
	return platform.IncomingMessage{
		Platform: "fake", EventID: "authz-" + strconv.FormatInt(authzEvent.Add(1), 10),
		UserID: user, ChatID: "chat-" + user, ChatType: chatType, Text: text,
		MentionMe: chatType == "group",
	}
}

func deniedCount(key string) int64 {
	if v, ok := dispatchDeniedTotal.Get(key).(interface{ Value() int64 }); ok {
		return v.Value()
	}
	return 0
}

// A refused direct message reaches neither a command arm nor a turn, and the
// sender is told once, with the ID to hand an operator; a second message
// inside the window gets no reply.
func TestAuthz_DeniedDirectMessage(t *testing.T) {
	d, fp, ct, _ := newAuthzDispatcher(t, aliceAndRoot())
	ctx := context.Background()
	before := deniedCount("fake:not_allowed")

	for _, text := range []string{"hello", "/urgent rm -rf ~", "/new"} {
		if _, ok := d.prepareInbound(ctx, authzMsg("eve", "direct", text)); ok {
			t.Fatalf("%q from eve was accepted", text)
		}
	}
	if n := ct.submits.Load() + ct.resets.Load(); n != 0 {
		t.Errorf("refused messages reached Turns %d times", n)
	}
	if got := fp.allReplies(); len(got) != 1 || !strings.Contains(got[0], "ID: eve") {
		t.Errorf("replies = %q, want exactly one refusal naming the sender ID", got)
	}
	// The throttle is per sender: eve's refusal must not silence the next stranger.
	if _, ok := d.prepareInbound(ctx, authzMsg("mallory", "direct", "hello")); ok {
		t.Fatal("hello from mallory was accepted")
	}
	if got := fp.allReplies(); len(got) != 2 || !strings.Contains(got[1], "ID: mallory") {
		t.Errorf("replies = %q, want a second refusal naming mallory", got)
	}
	if got := deniedCount("fake:not_allowed") - before; got != 4 {
		t.Errorf("naozhi_dispatch_denied_total[fake:not_allowed] moved by %d, want 4", got)
	}

	if _, ok := d.prepareInbound(ctx, authzMsg("alice", "direct", "hello")); !ok {
		t.Error("allowed user's message was refused")
	}
}

// In a group a stranger is dropped silently, so the bot cannot be made to spam
// the room; an allowed user's admin attempt is still answered.
func TestAuthz_GroupDenials(t *testing.T) {
	d, fp, _, cron := newAuthzDispatcher(t, aliceAndRoot())
	ctx := context.Background()

	if _, ok := d.prepareInbound(ctx, authzMsg("eve", "group", "hello")); ok {
		t.Fatal("stranger's group mention was accepted")
	}
	if n := fp.replyCount(); n != 0 {
		t.Errorf("stranger in a group got %d replies, want none", n)
	}
	if _, ok := d.prepareInbound(ctx, authzMsg("alice", "group", "/cron list")); ok {
		t.Fatal("non-admin /cron was accepted")
	}
	if cron.listJobsCalls != 0 {
		t.Error("non-admin /cron list reached the scheduler")
	}
	if got := fp.lastReply(); got != notAdminReply {
		t.Errorf("reply = %q, want %q", got, notAdminReply)
	}
}

// /cron, /cd and /project need an admin; /new and plain chat do not.
func TestAuthz_AdminCommands(t *testing.T) {
	d, fp, ct, cron := newAuthzDispatcher(t, aliceAndRoot())
	ctx := context.Background()
	before := deniedCount("fake:not_admin")

	for _, text := range []string{`/cron add "@every 1h" ping`, "/CRON del abc", "/cd /tmp", "/project off"} {
		fp.replies = nil
		if _, ok := d.prepareInbound(ctx, authzMsg("alice", "direct", text)); ok {
			t.Errorf("%q from a non-admin was accepted", text)
		}
		if got := fp.lastReply(); got != notAdminReply {
			t.Errorf("%q: reply = %q, want %q", text, got, notAdminReply)
		}
	}
	if cron.addJobCalls+cron.deleteJobCalls != 0 {
		t.Error("non-admin cron command reached the scheduler")
	}
	if ws := d.router.Workspace(sessionkey.ChatKey("fake", "direct", "chat-alice")); ws != "" {
		t.Errorf("non-admin /cd moved the workspace to %q", ws)
	}
	if got := deniedCount("fake:not_admin") - before; got != 4 {
		t.Errorf("naozhi_dispatch_denied_total[fake:not_admin] moved by %d, want 4", got)
	}

	d.prepareInbound(ctx, authzMsg("alice", "direct", "/new"))
	if ct.resets.Load() != 1 {
		t.Error("/new from an allowed user did not reset")
	}
	if _, ok := d.prepareInbound(ctx, authzMsg("alice", "direct", "hi")); !ok {
		t.Error("allowed user's chat was refused")
	}
	d.prepareInbound(ctx, authzMsg("root", "direct", "/cron list"))
	if cron.listJobsCalls != 1 {
		t.Error("admin /cron list did not reach the scheduler")
	}
}

// A Feishu AskUserQuestion card click is a synthesised group message with
// MentionMe set and the operator as UserID; it is judged like any other.
func TestAuthz_CardAnswerFromStrangerDenied(t *testing.T) {
	d, fp, ct, _ := newAuthzDispatcher(t, aliceAndRoot())
	msg := authzMsg("ou_eve", "group", "选项 A")
	msg.AgentID = "general"
	if _, ok := d.prepareInbound(context.Background(), msg); ok {
		t.Fatal("stranger's card answer was accepted")
	}
	if ct.submits.Load() != 0 || fp.replyCount() != 0 {
		t.Errorf("card answer: submits=%d replies=%d, want 0/0", ct.submits.Load(), fp.replyCount())
	}
}

// No policy, and a policy without a rule for the platform, leave behaviour
// as it was before im_access: everyone, even an empty UserID, is served.
func TestAuthz_UnconfiguredAllowsAll(t *testing.T) {
	for name, p := range map[string]*imauth.Policy{
		"nil":            nil,
		"other platform": {Rules: map[string]imauth.Rule{"slack": {Allowed: idSet("U1")}}},
	} {
		t.Run(name, func(t *testing.T) {
			d, _, _, cron := newAuthzDispatcher(t, p)
			if _, ok := d.prepareInbound(context.Background(), authzMsg("", "direct", "hello")); !ok {
				t.Error("message refused without a rule")
			}
			d.prepareInbound(context.Background(), authzMsg("anyone", "direct", "/cron list"))
			if cron.listJobsCalls != 1 {
				t.Error("/cron refused without a rule")
			}
		})
	}
}

func TestAuthz_DefaultDenyAndReplyOverride(t *testing.T) {
	d, fp, _, _ := newAuthzDispatcher(t, &imauth.Policy{DefaultDeny: true, DenyReply: "ask ops"})
	if _, ok := d.prepareInbound(context.Background(), authzMsg("alice", "direct", "hello")); ok {
		t.Fatal("default_deny accepted a sender on a platform with no rule")
	}
	if got := fp.lastReply(); got != "ask ops" {
		t.Errorf("reply = %q, want the configured deny_reply", got)
	}
}

// A sender with no ID cannot be onboarded, so it is refused without a reply.
func TestAuthz_EmptyUserSilent(t *testing.T) {
	d, fp, _, _ := newAuthzDispatcher(t, aliceAndRoot())
	if _, ok := d.prepareInbound(context.Background(), authzMsg("", "direct", "hello")); ok {
		t.Fatal("empty UserID accepted once a rule exists")
	}
	if n := fp.replyCount(); n != 0 {
		t.Errorf("empty UserID got %d replies, want none", n)
	}
}

func TestAuthz_SetAccessPolicyTakesEffect(t *testing.T) {
	d, _, _, _ := newAuthzDispatcher(t, nil)
	ctx := context.Background()
	if _, ok := d.prepareInbound(ctx, authzMsg("eve", "direct", "hi")); !ok {
		t.Fatal("open dispatcher refused")
	}
	d.SetAccessPolicy(aliceAndRoot())
	if _, ok := d.prepareInbound(ctx, authzMsg("eve", "direct", "hi")); ok {
		t.Error("swapped-in policy not applied to the next message")
	}
	d.SetAccessPolicy(nil)
	if _, ok := d.prepareInbound(ctx, authzMsg("eve", "direct", "hi")); !ok {
		t.Error("clearing the policy did not reopen the dispatcher")
	}
}

// Admit refuses a sender exactly as a text message would be refused: same
// metric, same throttled direct-chat reply naming the ID.
func TestAdmit_RefusesLikeText(t *testing.T) {
	d, fp, ct, _ := newAuthzDispatcher(t, aliceAndRoot())
	ctx := context.Background()
	before := deniedCount("fake:not_allowed")

	for range 2 {
		if d.Admit(ctx, authzMsg("eve", "direct", "")) {
			t.Fatal("Admit let a sender outside allowed_users through")
		}
	}
	if got := deniedCount("fake:not_allowed") - before; got != 2 {
		t.Errorf("naozhi_dispatch_denied_total[fake:not_allowed] moved by %d, want 2", got)
	}
	if got := fp.allReplies(); len(got) != 1 || !strings.Contains(got[0], "ID: eve") {
		t.Errorf("replies = %q, want one throttled refusal naming the sender ID", got)
	}
	if !d.Admit(ctx, authzMsg("alice", "direct", "")) {
		t.Error("Admit refused an allowed sender")
	}
	if ct.submits.Load() != 0 {
		t.Error("Admit submitted a turn")
	}
}

// Admit drops an un-mentioned group message before the policy is asked, so
// group chatter (and Feishu voice, which cannot @mention) is neither counted
// as a refusal nor fetched.
func TestAdmit_GroupMentionGate(t *testing.T) {
	d, fp, _, _ := newAuthzDispatcher(t, aliceAndRoot())
	ctx := context.Background()
	before := deniedCount("fake:not_allowed")

	for _, user := range []string{"alice", "eve"} {
		msg := authzMsg(user, "group", "")
		msg.MentionMe = false
		if d.Admit(ctx, msg) {
			t.Errorf("Admit let %s's un-mentioned group message through", user)
		}
	}
	if got := deniedCount("fake:not_allowed") - before; got != 0 {
		t.Errorf("un-mentioned group message counted %d refusals, want 0", got)
	}
	if fp.replyCount() != 0 {
		t.Error("un-mentioned group message was answered")
	}
	if !d.Admit(ctx, authzMsg("alice", "group", "")) {
		t.Error("Admit refused an allowed sender's mention")
	}
}

// Admit leaves dedup alone: the handler still processes the message it
// pre-checked, and an open dispatcher admits everyone.
func TestAdmit_DoesNotConsumeDedupAndNilPolicyAdmits(t *testing.T) {
	d, _, _, _ := newAuthzDispatcher(t, aliceAndRoot())
	ctx := context.Background()
	msg := authzMsg("alice", "direct", "hi")
	if !d.Admit(ctx, msg) {
		t.Fatal("Admit refused an allowed sender")
	}
	if _, ok := d.prepareInbound(ctx, msg); !ok {
		t.Error("the admitted message was dropped as a duplicate")
	}

	open, _, _, _ := newAuthzDispatcher(t, nil)
	if !open.Admit(ctx, authzMsg("", "direct", "")) {
		t.Error("Admit refused with no policy")
	}
}

func TestDenyThrottle(t *testing.T) {
	var th denyThrottle
	t0 := time.Unix(1_700_000_000, 0)
	if !th.allow("a", t0) {
		t.Fatal("first refusal not answered")
	}
	if th.allow("a", t0.Add(denyReplyWindow-time.Second)) {
		t.Error("second refusal inside the window answered")
	}
	if !th.allow("a", t0.Add(denyReplyWindow)) {
		t.Error("refusal after the window not answered")
	}

	var full denyThrottle
	for i := range denyReplyCap {
		full.allow(strconv.Itoa(i), t0)
	}
	if full.allow("new", t0.Add(time.Minute)) {
		t.Error("full table of live entries still answered a new sender")
	}
	if len(full.last) != denyReplyCap {
		t.Errorf("table grew to %d, cap %d", len(full.last), denyReplyCap)
	}
	if !full.allow("new", t0.Add(denyReplyWindow)) {
		t.Error("expired entries were not swept for a new sender")
	}
}

// commandClasses gives every slash command /help advertises the class it must
// get. TestClassifyCommand_CoversHelp fails when /help lists a command with
// no row here, so a new state-changing command cannot ship as Chat by
// omission.
var commandClasses = map[string]imauth.Class{
	"/cron":    imauth.Admin,
	"/cd":      imauth.Admin,
	"/project": imauth.Admin,
	"/help":    imauth.Chat,
	"/new":     imauth.Chat,
	"/clear":   imauth.Chat,
	"/stop":    imauth.Chat,
	"/urgent":  imauth.Chat,
	"/pwd":     imauth.Chat,
	// Tuning picks only choose among operator-configured backends / models
	// for the caller's own chat; they rebind nothing outside it.
	"/model":   imauth.Chat,
	"/effort":  imauth.Chat,
	"/backend": imauth.Chat,
}

func TestClassifyCommand_CoversHelp(t *testing.T) {
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.handleHelpCommand(context.Background(), incomingMsg("/help"))
	listed := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+(/[a-z]+)`).FindAllStringSubmatch(fp.lastReply(), -1) {
		listed[m[1]] = true
	}
	if len(listed) == 0 {
		t.Fatalf("no commands parsed from /help: %q", fp.lastReply())
	}
	var missing, stale []string
	for cmd := range listed {
		if _, ok := commandClasses[cmd]; !ok {
			missing = append(missing, cmd)
		}
	}
	for cmd := range commandClasses {
		if !listed[cmd] {
			stale = append(stale, cmd)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 || len(stale) > 0 {
		t.Errorf("/help and commandClasses differ: unclassified %q, stale %q", missing, stale)
	}

	for cmd, want := range commandClasses {
		for _, text := range []string{cmd, cmd + " arg", strings.ToUpper(cmd) + " arg", "  " + cmd + "  "} {
			if got := classifyCommand(text); got != want {
				t.Errorf("classifyCommand(%q) = %v, want %v", text, got, want)
			}
		}
	}
	for _, text := range []string{"hello", "/review code", "/cdx", "/cronjob", "/projects", "see /cron"} {
		if got := classifyCommand(text); got != imauth.Chat {
			t.Errorf("classifyCommand(%q) = %v, want chat", text, got)
		}
	}
}
