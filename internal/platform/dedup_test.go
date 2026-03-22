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
	// Cap reached, next insert rotates buckets: "a","b","c" move to previous
	d.Seen("d")

	// "a" should still be found in previous bucket (dual-bucket strategy)
	if !d.Seen("a") {
		t.Error("Seen(a) should return true (still in previous bucket)")
	}

	// Fill current bucket again to trigger second rotation
	d.Seen("e")
	d.Seen("f")
	// Now "a" was promoted to current when checked above, but "b","c" are gone
	// after this rotation: previous = {d, a (promoted), e, f}... actually let's
	// just verify the second rotation drops truly old entries
	d.Seen("g") // triggers rotation: current={d,a,e,f} -> previous, current={}
	d.Seen("h")
	d.Seen("i")
	d.Seen("j") // triggers rotation again: previous={g,h,i}, current={}

	// "b" has been rotated out twice — should not be seen
	if d.Seen("b") {
		t.Error("Seen(b) should return false after double rotation")
	}
}

func TestDedupDefaultCap(t *testing.T) {
	d := NewDedup(0)
	if d.cap != 10000 {
		t.Errorf("default cap = %d, want 10000", d.cap)
	}
}
