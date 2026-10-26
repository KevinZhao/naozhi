package platform_test

import (
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/platform/weixin"
)

// TestWeixinDeclaresSingleUseReplyToken locks weixin's own declarations behind
// #2136/#2204: its context_token is single-use, so it must answer
// SupportsInterimMessages()==false (an interim message would burn the token
// and lose the reply) and UsesSingleUseReplyToken()==true. The downstream
// collapse logic is tested with fakes; only this test asks the real type.
func TestWeixinDeclaresSingleUseReplyToken(t *testing.T) {
	t.Parallel()
	var p platform.Platform = &weixin.Weixin{}
	if _, ok := p.(platform.InterimMessageCapable); !ok {
		t.Fatal("weixin no longer implements InterimMessageCapable; the runtime-vs-static distinction this guards may be moot")
	}
	if platform.SupportsInterimMessages(p) {
		t.Error("weixin now supports interim messages; its single-use reply token would be consumed by the first one (#2136)")
	}
	if !platform.UsesSingleUseReplyToken(p) {
		t.Error("weixin no longer declares a single-use reply token; dispatch would stop collapsing its replies (#2136)")
	}
}
