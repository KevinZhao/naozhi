package config

import "fmt"

// IMRateLimitConfig is the im_rate_limit block: how many messages each IM
// sender may send. Absent, or msgs_per_min 0, leaves IM unlimited.
type IMRateLimitConfig struct {
	// MsgsPerMin is the sustained rate per sender; 0 disables the limit.
	MsgsPerMin int `yaml:"msgs_per_min,omitempty"`
	// Burst is how many messages a sender may send back to back; 0 means
	// MsgsPerMin.
	Burst int `yaml:"burst,omitempty"`
}

// validateIMRateLimit rejects negative values, and a burst with no rate: it
// would read as a limit while nothing is limited.
func validateIMRateLimit(cfg *Config) error {
	rl := cfg.IMRateLimit
	switch {
	case rl.MsgsPerMin < 0:
		return fmt.Errorf("im_rate_limit.msgs_per_min must be >= 0 (0 disables the limit), got %d", rl.MsgsPerMin)
	case rl.Burst < 0:
		return fmt.Errorf("im_rate_limit.burst must be >= 0 (0 means msgs_per_min), got %d", rl.Burst)
	case rl.Burst > 0 && rl.MsgsPerMin == 0:
		return fmt.Errorf("im_rate_limit.burst is %d but msgs_per_min is 0, which disables the limit; set msgs_per_min", rl.Burst)
	}
	return nil
}
