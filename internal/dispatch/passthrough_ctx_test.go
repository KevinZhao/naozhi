package dispatch

import (
	"context"
	"testing"
)

// TestSendOpts_PublicGettersRoundtrip pins R246-ARCH-10 (#786): the
// legacy WithPassthrough / WithUrgent / IsPassthrough / IsUrgent helpers
// must keep working after the internal collapse to a single sendOpts
// struct.  This guards against a regression where the consolidation
// stops one of the legacy callers (server.send / server.cron) from
// observing the bit it used to.
func TestSendOpts_PublicGettersRoundtrip(t *testing.T) {
	t.Parallel()

	t.Run("zero ctx → both false", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		if IsPassthrough(ctx) || IsUrgent(ctx) {
			t.Errorf("zero ctx: IsPassthrough=%v IsUrgent=%v, want both false",
				IsPassthrough(ctx), IsUrgent(ctx))
		}
	})

	t.Run("WithPassthrough alone", func(t *testing.T) {
		t.Parallel()
		ctx := WithPassthrough(context.Background())
		if !IsPassthrough(ctx) {
			t.Errorf("WithPassthrough(ctx) IsPassthrough=false, want true")
		}
		if IsUrgent(ctx) {
			t.Errorf("WithPassthrough(ctx) IsUrgent=true, want false (urgent must be opted in separately)")
		}
	})

	t.Run("WithUrgent alone", func(t *testing.T) {
		t.Parallel()
		ctx := WithUrgent(context.Background())
		if !IsUrgent(ctx) {
			t.Errorf("WithUrgent(ctx) IsUrgent=false, want true")
		}
		if IsPassthrough(ctx) {
			t.Errorf("WithUrgent(ctx) IsPassthrough=true, want false")
		}
	})

	t.Run("WithUrgent(WithPassthrough(...)) preserves both — legacy chain", func(t *testing.T) {
		t.Parallel()
		// This is the exact pattern used at internal/dispatch/commands.go:208
		// when an /urgent slash command lands.  The previous two-key shape
		// kept them as independent ctx.Value entries; the new struct merges
		// them via read-modify-write so chained Wraps must NOT clobber the
		// earlier bit.
		ctx := WithUrgent(WithPassthrough(context.Background()))
		if !IsPassthrough(ctx) {
			t.Errorf("Urgent(Passthrough(ctx)) lost Passthrough bit")
		}
		if !IsUrgent(ctx) {
			t.Errorf("Urgent(Passthrough(ctx)) lost Urgent bit")
		}
	})

	t.Run("WithPassthrough(WithUrgent(...)) preserves both — reverse order", func(t *testing.T) {
		t.Parallel()
		// Order independence: future call-sites may chain in either order.
		ctx := WithPassthrough(WithUrgent(context.Background()))
		if !IsPassthrough(ctx) || !IsUrgent(ctx) {
			t.Errorf("Passthrough(Urgent(ctx)) lost a bit: P=%v U=%v",
				IsPassthrough(ctx), IsUrgent(ctx))
		}
	})

	t.Run("nested derived ctx still observes ancestor opts", func(t *testing.T) {
		t.Parallel()
		// context.WithCancel / context.WithValue from the caller side must
		// not erase the dispatch decisions tagged on a parent ctx.  The
		// Go stdlib walks parents on Value(), so this should naturally
		// work — pin it so an over-eager refactor that adds a per-ctx
		// cache doesn't break it.
		parent := WithPassthrough(WithUrgent(context.Background()))
		child, cancel := context.WithCancel(parent)
		defer cancel()
		if !IsPassthrough(child) || !IsUrgent(child) {
			t.Errorf("derived ctx lost dispatch opts: P=%v U=%v",
				IsPassthrough(child), IsUrgent(child))
		}
	})
}
