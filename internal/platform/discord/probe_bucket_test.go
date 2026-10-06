package discord

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
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

// heldUsersBucket takes the /users bucket the way an in-flight request does
// and releases it at cleanup.
func heldUsersBucket(t *testing.T, sess *discordgo.Session) {
	t.Helper()
	b := sess.Ratelimiter.LockBucket(discordgo.EndpointUsers)
	t.Cleanup(func() { _ = b.Release(nil) })
}

// discordgo waits on the bucket mutex ignoring ctx, so a bucket held by
// another request must not stretch fetchSelf past probeTimeout (#3514).
func TestFetchSelf_HeldBucketHonoursTimeout(t *testing.T) {
	t.Parallel()
	sess, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	sess.Client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("request must not be sent")
	})}
	heldUsersBucket(t, sess)
	d := &Discord{probeTimeout: 50 * time.Millisecond}

	errc := make(chan error, 1)
	go func() { _, err := d.fetchSelf(sess); errc <- err }()
	select {
	case err := <-errc:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetchSelf waited on the held bucket past probeTimeout")
	}
}

// TestStop_NotHeldByBusyUsersBucket: a self-heal queued behind a held /users
// bucket must not hold Stop for its 30s handler wait.
func TestStop_NotHeldByBusyUsersBucket(t *testing.T) {
	t.Parallel()
	g := newFakeGateway(t, 0)
	d := newGatewayAdapter(t, g)
	d.probeTimeout = time.Hour
	startWithoutBotID(t, g, d)
	heldUsersBucket(t, d.session)

	d.maybeHealBotID()
	stopped := make(chan struct{})
	go func() { _ = d.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop waited on the self-heal queued behind the bucket")
	}
}

// The bucket check runs while the heal and the disconnect probe may both be
// in flight; under -race it must not read the bucket a concurrent request is
// updating.
func TestFetchSelf_ConcurrentCallsRaceFree(t *testing.T) {
	t.Parallel()
	for range 20 {
		sess, err := discordgo.New("Bot test-token")
		if err != nil {
			t.Fatal(err)
		}
		sess.Client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			h := http.Header{}
			h.Set("Content-Type", "application/json")
			h.Set("X-RateLimit-Remaining", "5")
			h.Set("X-RateLimit-Reset-After", "1")
			return &http.Response{StatusCode: http.StatusOK, Header: h, Request: r,
				Body: io.NopCloser(strings.NewReader(`{"id":"bot-1"}`))}, nil
		})}
		d := &Discord{probeTimeout: 5 * time.Second}
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				if _, err := d.fetchSelf(sess); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
