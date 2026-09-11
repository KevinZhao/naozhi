package clievent

import (
	"testing"
)

func TestSinceInclusive(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want int64 }{
		{0, 0},
		{-5, -5},
		{1, 0},
		{1700000000000, 1699999999999},
	}
	for _, c := range cases {
		if got := SinceInclusive(c.in); got != c.want {
			t.Errorf("SinceInclusive(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
