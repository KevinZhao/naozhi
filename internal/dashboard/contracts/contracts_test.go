package contracts

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsUnknownRPCMethodErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("connection reset"), false},
		{"bare", errors.New("unknown method: remove_session"), true},
		{"wrapped", fmt.Errorf("proxy: %w", fmt.Errorf("rpc: %w", errors.New("unknown method: x"))), true},
		{"case sensitive", errors.New("Unknown Method"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUnknownRPCMethodErr(tc.err); got != tc.want {
				t.Fatalf("IsUnknownRPCMethodErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
