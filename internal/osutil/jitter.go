package osutil

import (
	"math/rand/v2"
	"time"
)

// JitterBackoff returns d scaled by a random factor in [0.75, 1.25) so a
// fleet of reconnect loops restarted on the same second do not all fire on
// identical deadlines. math/rand/v2 uses a per-goroutine source, so
// concurrent callers do not contend on a global lock.
//
// Used by platform reconnect loops, the websocket node relay, and the
// upstream connector. Keep the range symmetric around 1.0 so the mean
// backoff stays at d even under heavy retry churn.
func JitterBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	// Float64 returns [0,1); remap to [0.75, 1.25).
	factor := 0.75 + rand.Float64()*0.5
	return time.Duration(float64(d) * factor)
}
