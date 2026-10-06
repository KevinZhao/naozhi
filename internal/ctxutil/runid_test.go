package ctxutil

import (
	"context"
	"testing"
)

func TestRunIDAndSessionKey_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var nilCtx context.Context
	if RunID(ctx) != "" || SessionKey(ctx) != "" || RunID(nilCtx) != "" {
		t.Fatal("empty ctx should carry nothing")
	}
	if WithRunID(ctx, "") != ctx || WithSessionKey(ctx, "") != ctx {
		t.Fatal("empty values must not allocate a new ctx")
	}
	ctx = WithSessionKey(WithRunID(ctx, "r1"), "feishu:direct:a:general")
	if RunID(ctx) != "r1" || SessionKey(ctx) != "feishu:direct:a:general" {
		t.Fatalf("got %q / %q", RunID(ctx), SessionKey(ctx))
	}
}
