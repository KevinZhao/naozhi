package dispatch

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

const (
	// rateLimitMaxKeys bounds the sender buckets; the least recently seen
	// sender is evicted first.
	rateLimitMaxKeys = 4096
	// rateLimitIdleTTL resets a bucket whose sender has been quiet this long.
	rateLimitIdleTTL = 10 * time.Minute
	// rateLimitReplyWindow is how long a limited sender waits for the next
	// "too fast" reply; the messages in between are dropped silently.
	rateLimitReplyWindow = time.Minute
)

// RateLimit caps how many IM messages each sender may send. MsgsPerMin 0
// disables it; Burst 0 means MsgsPerMin.
type RateLimit struct {
	MsgsPerMin int
	Burst      int
}

// newInboundLimiter builds the per-sender token bucket for rl, or nil when
// the limit is off.
func newInboundLimiter(rl RateLimit) *ratelimit.Limiter {
	if rl.MsgsPerMin <= 0 {
		return nil
	}
	burst := rl.Burst
	if burst <= 0 {
		burst = rl.MsgsPerMin
	}
	return ratelimit.New(ratelimit.Config{
		Rate:    rate.Limit(float64(rl.MsgsPerMin) / 60),
		Burst:   burst,
		MaxKeys: rateLimitMaxKeys,
		TTL:     rateLimitIdleTTL,
	})
}

// rateLimitKey is msg's bucket: the sender on its platform, or the chat when
// the platform gave no sender ID. Never empty, which Limiter.Allow refuses.
func rateLimitKey(msg platform.IncomingMessage) string {
	if msg.UserID == "" {
		return "chat\x00" + sessionkey.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
	}
	return "user\x00" + msg.Platform + "\x00" + msg.UserID
}

func isStopCommand(trimmed string) bool {
	return trimmed == "/stop" || strings.HasPrefix(trimmed, "/stop ")
}

// admitRate reports whether msg fits its sender's rate limit. /stop is never
// limited: it ends spend. A limited message is counted and dropped; the
// sender is told at most once per rateLimitReplyWindow.
func (d *Dispatcher) admitRate(ctx context.Context, msg platform.IncomingMessage, trimmed string, lg *slog.Logger) bool {
	if d.inboundLimit == nil || isStopCommand(trimmed) {
		return true
	}
	key := rateLimitKey(msg)
	if d.inboundLimit.Allow(key) {
		return true
	}
	dispatchRateLimitedTotal.Add(1)
	if !d.rateLimitReplies.allow(key, time.Now()) {
		lg.Debug("im message rate limited")
		return false
	}
	lg.Info("im message rate limited", "msgs_per_min", d.rateLimit.MsgsPerMin)
	d.replyText(ctx, msg, "消息过于频繁（每分钟最多 "+strconv.Itoa(d.rateLimit.MsgsPerMin)+" 条），请稍后再试。", lg)
	return false
}
