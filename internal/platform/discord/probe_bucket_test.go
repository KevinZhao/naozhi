package discord

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fetchSelf must not enter discordgo's pre-request bucket sleep (which
// ignores ctx and would hold Stop): an exhausted /users bucket makes it
// return errProbeRateLimited at once without a request (#3514).
func TestFetchSelf_SkipsWhenBucketExhausted(t *testing.T) {
	t.Parallel()
	sess, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	// A client that refuses every request: if fetchSelf gets past the bucket
	// check the error is a transport one, not errProbeRateLimited.
	sess.Client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("request must not be sent")
	})}
	d := &Discord{probeTimeout: 2 * time.Second}

	// Exhaust the bucket through discordgo's own header parser, the way a
	// real response would.
	b := sess.Ratelimiter.LockBucket(discordgo.EndpointUsers)
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset-After", "30")
	if err := b.Release(h); err != nil {
		t.Fatal(err)
	}
	if wait := sess.Ratelimiter.GetWaitTime(sess.Ratelimiter.GetBucket(discordgo.EndpointUsers), 1); wait <= 0 {
		t.Fatalf("fixture: bucket not exhausted, wait = %v", wait)
	}

	start := time.Now()
	_, err = d.fetchSelf(sess)
	if !errors.Is(err, errProbeRateLimited) {
		t.Fatalf("err = %v, want errProbeRateLimited", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("fetchSelf slept on the bucket")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
