package wireup

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

const (
	adoptE2EAssistant  = `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]},"session_id":"s1"}`
	adoptE2EPrevResult = `{"type":"result","subtype":"success","result":"previous run","session_id":"s1"}`
)

// reattachToFakeShim reconnects a router to a fake shim for key that replays
// backlog from firstSeq, leaving the key's session on a real *cli.Process.
func reattachToFakeShim(t *testing.T, key string, firstSeq int64, backlog []string) *session.Router {
	t.Helper()
	fs := session.StartFakeShimForTest(t, key, firstSeq, backlog)
	r := session.NewRouter(fs.Config)
	t.Cleanup(r.Shutdown)
	r.ReconnectShimsCtx(context.Background())
	fs.Attached(t)
	return r
}

// fakeShimMark is the marker string a run on the fake shim records at seq.
func fakeShimMark(seq int64) string {
	return cli.TurnWatermark{ShimPID: session.FakeShimPID, Seq: seq}.String()
}

// TestCronRouterAdapter_AdoptInFlight_HonoursMarkerWatermark drives the marker's
// watermark string through the adapter into the gate on a real reattached
// process. The backlog's last result sits at seq 8, so a watermark below it on
// the same shim, or one from another shim, adopts it; one at it, or none, does
// not. Each verdict must match a direct router call with the parsed watermark.
func TestCronRouterAdapter_AdoptInFlight_HonoursMarkerWatermark(t *testing.T) {
	const key = "cron:3c9e1f07a5d24b68"
	for _, tc := range []struct {
		name string
		mark string
		want cron.AdoptVerdict
	}{
		{"before this run's result", fakeShimMark(5), cron.AdoptLive},
		{"one before this run's result", fakeShimMark(7), cron.AdoptLive},
		{"at this run's result", fakeShimMark(8), cron.AdoptNone},
		{"from a shim since replaced", cli.TurnWatermark{ShimPID: 4242, Seq: 8}.String(), cron.AdoptLive},
		{"no watermark recorded", "", cron.AdoptNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := reattachToFakeShim(t, key, 5, []string{
				adoptE2EPrevResult,
				`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"do thing"}]},"session_id":"s1"}`,
				adoptE2EAssistant,
				`{"type":"result","subtype":"success","result":"this run","session_id":"s1"}`,
			})

			run, got := cronRouterAdapter{r: r}.AdoptInFlight(key, tc.mark)
			w, known := cli.ParseTurnWatermark(tc.mark)
			_, direct := r.AdoptInFlight(key, w, known)
			if got != tc.want || got != cron.AdoptVerdict(int(direct)) {
				t.Fatalf("AdoptInFlight(%q) = %v, want %v (router called directly: %v)", tc.mark, got, tc.want, direct)
			}
			if tc.want != cron.AdoptLive {
				if run != nil {
					t.Errorf("AdoptInFlight(%q) run = %v, want nil with %v", tc.mark, run, got)
				}
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, err := run.AwaitAdopted(ctx)
			if err != nil || !out.Completed || out.Text != "this run" {
				t.Errorf("AwaitAdopted = (%+v, %v), want this run's result", out, err)
			}
		})
	}
}

// TestCronSessionAdapter_SendWatermarkRoundTripsThroughAdopter: what
// SendWatermark records on an idle reattached session is what AdoptInFlight
// parses back. That watermark covers the replayed result; one a seq earlier on
// the same shim does not.
func TestCronSessionAdapter_SendWatermarkRoundTripsThroughAdopter(t *testing.T) {
	const key = "cron:b72d40e8c61f9a35"
	r := reattachToFakeShim(t, key, 1, []string{adoptE2EAssistant, adoptE2EPrevResult})

	mark := cronSessionAdapter{s: r.SessionFor(key)}.SendWatermark()
	if want := strconv.Itoa(session.FakeShimPID) + ":2"; mark != want {
		t.Fatalf("SendWatermark = %q, want %q", mark, want)
	}
	adopter := cronRouterAdapter{r: r}
	if run, v := adopter.AdoptInFlight(key, mark); v != cron.AdoptNone || run != nil {
		t.Errorf("AdoptInFlight(%q) = (%v, %v), want (nil, AdoptNone): the replayed result is already covered", mark, run, v)
	}
	if run, v := adopter.AdoptInFlight(key, fakeShimMark(1)); v != cron.AdoptLive || run == nil {
		t.Errorf("AdoptInFlight(%q) = (%v, %v), want AdoptLive for the result past it", fakeShimMark(1), run, v)
	}
}
