package history

import (
	"context"
	"testing"
)

func TestNoopSource_AlwaysReturnsNil(t *testing.T) {
	t.Parallel()
	var s Source = Noop{}
	cases := []struct {
		name   string
		before int64
		limit  int
	}{
		{"zero_before", 0, 10},
		{"positive_before", 12345, 10},
		{"limit_zero", 0, 0},
		{"negative_limit", 100, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries, err := s.LoadBefore(context.Background(), tc.before, tc.limit)
			if err != nil {
				t.Errorf("Noop.LoadBefore returned error: %v", err)
			}
			if entries != nil {
				t.Errorf("Noop.LoadBefore returned %d entries, want nil", len(entries))
			}
		})
	}
}
