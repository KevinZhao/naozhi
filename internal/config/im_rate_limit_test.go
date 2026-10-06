package config

import (
	"strings"
	"testing"
)

// A negative value, or a burst with no rate, is fatal: either would load as a
// limit the dispatcher does not enforce the way it reads.
func TestLoad_IMRateLimitRefusals(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"negative rate", "im_rate_limit:\n  msgs_per_min: -1\n", "im_rate_limit.msgs_per_min must be >= 0"},
		{"negative burst", "im_rate_limit:\n  msgs_per_min: 5\n  burst: -2\n", "im_rate_limit.burst must be >= 0"},
		{"burst without rate", "im_rate_limit:\n  burst: 3\n", "im_rate_limit.burst is 3 but msgs_per_min is 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			if err == nil {
				t.Fatalf("Load accepted:\n%s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoad_IMRateLimit(t *testing.T) {
	for _, tc := range []struct {
		body       string
		rate, brst int
	}{
		{"", 0, 0},
		{"im_rate_limit:\n  msgs_per_min: 10\n", 10, 0},
		{"im_rate_limit:\n  msgs_per_min: 10\n  burst: 3\n", 10, 3},
	} {
		cfg, err := Load(writeCfg(t, tc.body))
		if err != nil {
			t.Fatalf("Load(%q): %v", tc.body, err)
		}
		if got := cfg.IMRateLimit; got.MsgsPerMin != tc.rate || got.Burst != tc.brst {
			t.Errorf("Load(%q).IMRateLimit = %+v, want msgs_per_min %d burst %d", tc.body, got, tc.rate, tc.brst)
		}
	}
}
