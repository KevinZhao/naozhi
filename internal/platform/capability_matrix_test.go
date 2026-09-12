package platform_test

import (
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/platform/discord"
	"github.com/naozhi/naozhi/internal/platform/feishu"
	"github.com/naozhi/naozhi/internal/platform/slack"
	"github.com/naozhi/naozhi/internal/platform/weixin"
)

// TestCapabilityMatrixIsPinned is the gate the matrix exists for: today nothing
// notices when a platform silently loses a capability — dropping Reactor from
// slack would compile, pass every test, and just stop marking queued messages.
// The expected rows below are MEASURED, not aspirational; a diff here means
// either a real regression or a deliberate change that must be recorded in the
// same commit.
//
// Adapters are zero-valued on purpose: every capability except MaxReplyLength is
// a property of the TYPE, so no credentials or transport are needed to ask.
func TestCapabilityMatrixIsPinned(t *testing.T) {
	t.Parallel()
	want := map[string]platform.Capabilities{
		"discord": {
			InterimMessages: true, SingleUseReplyToken: false,
			Reactions: true, QuestionCards: false, Runnable: true,
		},
		// Feishu is the only platform with native AskUserQuestion cards;
		// everywhere else dispatch falls back to a plain-text option list.
		"feishu": {
			InterimMessages: true, SingleUseReplyToken: false,
			Reactions: true, QuestionCards: true, Runnable: true,
		},
		"slack": {
			InterimMessages: true, SingleUseReplyToken: false,
			Reactions: true, QuestionCards: false, Runnable: true,
		},
		// Weixin is the single-use-reply-token platform (#2136) and the only one
		// without reactions. It also IMPLEMENTS InterimMessageCapable while
		// answering false, which is why the matrix asks at runtime.
		"weixin": {
			InterimMessages: false, SingleUseReplyToken: true,
			Reactions: false, QuestionCards: false, Runnable: true,
		},
	}
	got := platform.CapabilityMatrix(map[string]platform.Platform{
		"discord": &discord.Discord{},
		"feishu":  &feishu.Feishu{},
		"slack":   &slack.Slack{},
		"weixin":  &weixin.Weixin{},
	})
	if len(got) != len(want) {
		t.Fatalf("matrix has %d rows, want %d: %+v", len(got), len(want), got)
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s: missing from the matrix", name)
			continue
		}
		// MaxReplyLength depends on config, not on the type, so it is not pinned.
		g.MaxReplyLength = 0
		if g != w {
			t.Errorf("%s capabilities changed:\n got %+v\nwant %+v", name, g, w)
		}
	}
}

// TestWeixinDeclaresInterimCapabilityButDeclines is the reason CapabilitiesOf
// asks at runtime instead of type-asserting. If weixin ever starts returning
// true, this test says so out loud rather than the matrix quietly changing.
func TestWeixinDeclaresInterimCapabilityButDeclines(t *testing.T) {
	t.Parallel()
	var p platform.Platform = &weixin.Weixin{}
	if _, ok := p.(platform.InterimMessageCapable); !ok {
		t.Fatal("weixin no longer implements InterimMessageCapable; the runtime-vs-static distinction this guards may be moot")
	}
	if platform.SupportsInterimMessages(p) {
		t.Error("weixin now supports interim messages; update the pinned matrix in the same commit")
	}
}

// TestCapabilitiesOfNilIsAllFalse: a platform that failed to build must read as
// "can do nothing", never as "can do everything".
func TestCapabilitiesOfNilIsAllFalse(t *testing.T) {
	t.Parallel()
	if got := platform.CapabilitiesOf(nil); got != (platform.Capabilities{}) {
		t.Errorf("CapabilitiesOf(nil) = %+v, want the zero value", got)
	}
}

// TestCapabilityMatrixKeepsNilEntries: "configured but unusable" is the signal an
// operator needs, so the row stays with everything false instead of vanishing.
func TestCapabilityMatrixKeepsNilEntries(t *testing.T) {
	t.Parallel()
	got := platform.CapabilityMatrix(map[string]platform.Platform{"broken": nil})
	row, ok := got["broken"]
	if !ok {
		t.Fatalf("nil platform dropped from the matrix: %+v", got)
	}
	if row != (platform.Capabilities{}) {
		t.Errorf("got %+v, want the zero value", row)
	}
}

// TestCapabilityMatrixEmptyIsNil keeps the /health field omitempty-friendly.
func TestCapabilityMatrixEmptyIsNil(t *testing.T) {
	t.Parallel()
	if got := platform.CapabilityMatrix(nil); got != nil {
		t.Errorf("CapabilityMatrix(nil) = %+v, want nil", got)
	}
}
