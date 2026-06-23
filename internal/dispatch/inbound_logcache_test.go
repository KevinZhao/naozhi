package dispatch

import (
	"log/slog"
	"strconv"
	"testing"
)

func TestInboundLogCache_ReusesLogger(t *testing.T) {
	t.Parallel()
	var c inboundLogCache
	if got := c.get("k"); got != nil {
		t.Fatalf("empty cache get = %v, want nil", got)
	}
	lg := slog.Default().With("x", 1)
	c.put("k", lg)
	if got := c.get("k"); got != lg {
		t.Fatalf("get after put = %v, want same logger %v", got, lg)
	}
	// Second get must return the identical pointer (no rebuild).
	if c.get("k") != lg {
		t.Fatal("get is not stable across calls")
	}
}

// TestInboundLogCache_BoundedDrop verifies the map is dropped and rebuilt once
// it crosses the entry cap, so a dispatcher serving unbounded distinct chats
// cannot leak memory (#2233).
func TestInboundLogCache_BoundedDrop(t *testing.T) {
	t.Parallel()
	var c inboundLogCache
	lg := slog.Default()
	for i := 0; i < inboundLogMaxEntries; i++ {
		c.put(strconv.Itoa(i), lg)
	}
	c.mu.RLock()
	full := len(c.m)
	c.mu.RUnlock()
	if full != inboundLogMaxEntries {
		t.Fatalf("len before overflow = %d, want %d", full, inboundLogMaxEntries)
	}
	// One more put crosses the cap and triggers a rebuild.
	c.put("overflow", lg)
	c.mu.RLock()
	after := len(c.m)
	c.mu.RUnlock()
	if after != 1 {
		t.Fatalf("len after overflow = %d, want 1 (rebuilt)", after)
	}
	if c.get("overflow") != lg {
		t.Fatal("overflow entry missing after rebuild")
	}
}
