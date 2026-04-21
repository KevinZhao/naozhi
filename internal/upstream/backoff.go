package upstream

import (
	"math/rand/v2"
	"time"
)

// jitterBackoff returns d scaled by a random factor in [0.75, 1.25) so
// fleets of connectors restarted together do not reconnect on aligned
// deadlines. math/rand/v2 uses a per-goroutine source, so concurrent
// callers don't contend on a global lock.
func jitterBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	factor := 0.75 + rand.Float64()*0.5
	return time.Duration(float64(d) * factor)
}
