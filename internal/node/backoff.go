package node

import (
	"math/rand/v2"
	"time"
)

// jitterBackoff returns d scaled by a random factor in [0.75, 1.25). Used by
// reconnect loops (wsRelay.reconnect) so many clients reconnecting to the
// same primary after a restart don't all fire at identical offsets.
// math/rand/v2 uses a per-goroutine source so concurrent callers do not
// contend on a global lock.
func jitterBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	factor := 0.75 + rand.Float64()*0.5
	return time.Duration(float64(d) * factor)
}
