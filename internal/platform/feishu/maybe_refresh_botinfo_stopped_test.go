package feishu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestMaybeRefreshBotInfo_SkipsAfterStop pins R20260531-CR-003: once stopCtx is
// cancelled (Feishu.Stop has called stopCancel and is about to block on
// wg.Wait), maybeRefreshBotInfo MUST NOT launch the self-heal goroutine.
//
// Original bug: a webhook goroutine could win the CAS and call wg.Add(1) after
// Stop's wg.Wait had already drained the counter to 0; the goroutine's
// deferred wg.Done then drives the counter negative and panics. The fix
// re-checks stopCtx.Err() after the CAS and before wg.Add, returning early.
//
// Discriminator: we hold the bot_info singleflight key busy with a blocking
// call before invoking maybeRefreshBotInfo. If (and only if) the self-heal
// goroutine launches, its botInfoSF.Do("bot_info", …) dedups onto the held
// call and blocks — so f.wg never reaches 0 and wg.Wait hangs. With the fix
// no goroutine launches, wg stays 0, and wg.Wait returns immediately.
func TestMaybeRefreshBotInfo_SkipsAfterStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &Feishu{stopCtx: ctx, stopCancel: cancel, baseURL: "http://127.0.0.1:0"}

	// Hold the singleflight key busy so any launched self-heal goroutine
	// would block inside botInfoSF.Do rather than racing to completion.
	release := make(chan struct{})
	sfBusy := make(chan struct{})
	go func() {
		_, _, _ = f.botInfoSF.Do("bot_info", func() (any, error) {
			close(sfBusy)
			<-release
			return nil, nil
		})
	}()
	<-sfBusy
	defer close(release)

	atomic.StoreInt64(&f.lastBotInfoFetchNs, 0) // open the cooldown gate
	cancel()                                    // simulate Stop(): stopCtx cancelled

	f.maybeRefreshBotInfo() // must not panic, must not launch the goroutine

	// wg must be 0: a launched goroutine would be parked in the busy
	// singleflight, so wg.Wait would block and trip the timeout.
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wg.Wait blocked — self-heal goroutine was launched despite cancelled stopCtx")
	}
}

// TestMaybeRefreshBotInfo_RunsWhenLive is the positive control: with a live
// stopCtx, the self-heal goroutine launches and the fetch fires. Guards the
// early-return from being too aggressive and skipping the live path.
func TestMaybeRefreshBotInfo_RunsWhenLive(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		w.Write([]byte(`{"code":0,"bot":{"open_id":"ou_live"}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &Feishu{stopCtx: ctx, stopCancel: cancel, baseURL: srv.URL}
	f.accessToken = "tok"
	f.tokenExpiry = time.Now().Add(time.Hour)
	atomic.StoreInt64(&f.lastBotInfoFetchNs, 0)

	f.maybeRefreshBotInfo()

	select {
	case <-hit:
	case <-time.After(2 * time.Second):
		t.Fatal("live stopCtx: self-heal fetch was never issued")
	}
	f.wg.Wait()
}
