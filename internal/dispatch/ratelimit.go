package dispatch

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/ratelimit"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/turn"
)

const (
	// rateLimitMaxKeys bounds the sender buckets; the least recently seen
	// sender is evicted first.
	rateLimitMaxKeys = 4096
	// rateLimitIdleTTL is the shortest quiet spell that resets a bucket;
	// inboundLimitConfig stretches it to the bucket's full refill time.
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

// inboundLimit is an enabled RateLimit with its bucket.
type inboundLimit struct {
	policy  RateLimit
	limiter *ratelimit.Limiter
}

// newInboundLimit builds the per-sender token bucket for rl, or nil when the
// limit is off.
func newInboundLimit(rl RateLimit) *inboundLimit {
	if rl.MsgsPerMin <= 0 {
		return nil
	}
	return &inboundLimit{policy: rl, limiter: ratelimit.New(inboundLimitConfig(rl))}
}

// SetRateLimit replaces the per-sender rate limit; the next message draws
// from a fresh bucket. The zero value turns the limit off.
func (d *Dispatcher) SetRateLimit(rl RateLimit) { d.inbound.Store(newInboundLimit(rl)) }

// inboundLimitConfig is the bucket for an enabled rl. Its idle TTL is at
// least the time to refill the burst, so a reset never hands back more
// than waiting would.
func inboundLimitConfig(rl RateLimit) ratelimit.Config {
	burst := rl.Burst
	if burst <= 0 {
		burst = rl.MsgsPerMin
	}
	ttl := time.Duration(math.MaxInt64)
	if int64(burst) <= int64(math.MaxInt64/time.Minute) {
		ttl = max(rateLimitIdleTTL, time.Duration(burst)*time.Minute/time.Duration(rl.MsgsPerMin))
	}
	return ratelimit.Config{
		Rate:    rate.Limit(float64(rl.MsgsPerMin) / 60),
		Burst:   burst,
		MaxKeys: rateLimitMaxKeys,
		TTL:     ttl,
	}
}

// rateLimitKey is msg's bucket: the sender on its platform, or the chat when
// the platform gave no sender ID. Never empty, which Limiter.Allow refuses.
func rateLimitKey(msg platform.IncomingMessage) string {
	if msg.UserID == "" {
		return "chat\x00" + sessionkey.ChatKey(msg.Platform, msg.ChatType, msg.ChatID)
	}
	return "user\x00" + msg.Platform + "\x00" + msg.UserID
}

// isStopCommand reports whether trimmed is /stop as dispatchCommand reads
// it: the command token normalized by turn.NormalizeCommand.
func isStopCommand(trimmed string) bool {
	t := turn.NormalizeCommand(trimmed)
	return t == "/stop" || strings.HasPrefix(t, "/stop ")
}

// admitRate reports whether msg fits its sender's rate limit. /stop is never
// limited: it ends spend. A limited message is counted and dropped; the
// sender is told at most once per rateLimitReplyWindow.
func (d *Dispatcher) admitRate(ctx context.Context, msg platform.IncomingMessage, trimmed string, lg *slog.Logger) bool {
	in := d.inbound.Load()
	if in == nil || isStopCommand(trimmed) {
		return true
	}
	key := rateLimitKey(msg)
	if in.limiter.Allow(key) {
		return true
	}
	dispatchRateLimitedTotal.Add(1)
	if !d.rateLimitReplies.allow(key, time.Now()) {
		lg.DebugContext(ctx, "im message rate limited")
		return false
	}
	lg.InfoContext(ctx, "im message rate limited", "msgs_per_min", in.policy.MsgsPerMin)
	d.replyText(ctx, msg, "消息过于频繁（每分钟最多 "+strconv.Itoa(in.policy.MsgsPerMin)+" 条），请稍后再试。", lg)
	return false
}
