package turn

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/ctxutil"
)

func TestWithTurnIDs(t *testing.T) {
	t.Parallel()
	ctx := withTurnIDs(ctxutil.WithTraceID(context.Background(), "tr"), "feishu:direct:a:general")
	if ctxutil.TraceID(ctx) != "tr" {
		t.Fatal("trace id lost")
	}
	if ctxutil.SessionKey(ctx) != "feishu:direct:a:general" {
		t.Fatalf("session key = %q", ctxutil.SessionKey(ctx))
	}
	if id := ctxutil.RunID(ctx); len(id) != 16 {
		t.Fatalf("run id = %q", id)
	}
	if ctxutil.RunID(withTurnIDs(ctx, "k")) == ctxutil.RunID(ctx) {
		t.Fatal("each turn must mint its own run id")
	}
}
