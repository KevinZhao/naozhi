package osutil

import (
	"math/rand/v2"
	"time"
)

// JitterBackoff returns d scaled by a random factor in [0.75, 1.25) so
// reconnect loops restarted on the same second do not fire on identical
// deadlines. The range is symmetric around 1.0 so the mean stays at d.
func JitterBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	// Float64 returns [0,1); remap to [0.75, 1.25).
	factor := 0.75 + rand.Float64()*0.5
	return time.Duration(float64(d) * factor)
}
