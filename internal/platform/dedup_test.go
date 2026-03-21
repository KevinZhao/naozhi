package platform

import "testing"

func TestDedup(t *testing.T) {
	d := NewDedup(3)

	// First time: not seen
	if d.Seen("a") {
		t.Error("Seen(a) should return false on first call")
	}
	// Second time: seen
	if !d.Seen("a") {
		t.Error("Seen(a) should return true on second call")
	}
	// Empty ID: never seen
	if d.Seen("") {
		t.Error("Seen(\"\") should always return false")
	}
}

func TestDedupEviction(t *testing.T) {
	d := NewDedup(3)
	d.Seen("a")
	d.Seen("b")
	d.Seen("c")
	// Cap reached, next insert clears
	d.Seen("d")

	// "a" should no longer be seen after eviction
	if d.Seen("a") {
		t.Error("Seen(a) should return false after eviction")
	}
}

func TestDedupDefaultCap(t *testing.T) {
	d := NewDedup(0)
	if d.cap != 10000 {
		t.Errorf("default cap = %d, want 10000", d.cap)
	}
}
