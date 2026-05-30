package cron

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/platform"
)

// buildDistinctChunks returns a string that SplitText(s, chunkSize) will
// chop into n chunks each with distinct text — necessary because our
// fake counts unique chunk Texts (not raw Reply invocations) so that
// ReplyWithRetry's per-chunk retry calls don't double-count. Each chunk
// gets a unique two-digit prefix followed by enough filler to hit
// chunkSize. SplitText prefers newline-as-boundary in the second half
// of a slot, so we insert a newline at every chunkSize-th position.
func buildDistinctChunks(n, chunkSize int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		// e.g. chunkSize=8 -> "00abcdef\n", "01abcdef\n", ...
		b.WriteString(strings.Repeat("0", 2-len("0"+string(rune('0'+i/10)))))
		b.WriteByte(byte('0' + (i / 10)))
		b.WriteByte(byte('0' + (i % 10)))
		// Fill the rest of this chunk with filler chars; -1 to leave room
		// for the trailing newline so SplitText splits exactly here.
		filler := chunkSize - 2 - 1
		if filler < 0 {
			filler = 0
		}
		b.WriteString(strings.Repeat("x", filler))
		b.WriteByte('\n')
	}
	return b.String()
}

// fakePartialPlatform is a minimal platform.Platform implementation used to
// drive notifyTarget's chunk loop. We track distinct chunk texts seen via
// chunksSent so the test can assert the loop aborts after a chunk fails
// rather than moving on to the next one. (sendCount alone double-counts
// ReplyWithRetry's per-chunk retries — what we care about here is "did
// the next chunk get attempted at all?", which is a per-text question.)
type fakePartialPlatform struct {
	failAt    int // chunk index (0-based) at which Reply starts failing
	maxLen    int
	sendCount atomic.Int32 // total Reply invocations including retries
	mu        sync.Mutex
	seenTexts map[string]struct{} // unique chunk Texts handed to Reply
}

func (f *fakePartialPlatform) Name() string { return "fake-notify" }
func (f *fakePartialPlatform) RegisterRoutes(*http.ServeMux, platform.MessageHandler) {
}
func (f *fakePartialPlatform) Reply(_ context.Context, msg platform.OutgoingMessage) (string, error) {
	f.sendCount.Add(1)
	f.mu.Lock()
	if f.seenTexts == nil {
		f.seenTexts = map[string]struct{}{}
	}
	f.seenTexts[msg.Text] = struct{}{}
	uniq := len(f.seenTexts)
	f.mu.Unlock()
	// uniq counts chunks attempted (1, 2, 3, …); failAt is the 0-based index
	// at which we start returning an error. Index 2 == third unique chunk.
	if uniq-1 >= f.failAt {
		return "", errors.New("simulated send failure")
	}
	return "msg-id", nil
}
func (f *fakePartialPlatform) EditMessage(context.Context, string, string) error { return nil }
func (f *fakePartialPlatform) MaxReplyLength() int                               { return f.maxLen }
func (f *fakePartialPlatform) uniqueChunks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seenTexts)
}

// TestR250CR18_NotifyTargetAbortsOnFirstChunkFailure pins the #1151 contract:
// once any chunk fails the loop must stop pushing subsequent chunks. Pre-fix
// behaviour was "log WARN, continue" which left the user reading a sliced
// message interleaved with foreign chat traffic; post-fix is "log one
// aggregated WARN with delivered/total, abort". We assert sendCount stops
// climbing once Reply returns its first error.
func TestR250CR18_NotifyTargetAbortsOnFirstChunkFailure(t *testing.T) {
	t.Parallel()
	// Force SplitText to chop the input into many chunks by setting maxLen to
	// 8 chars and supplying ~80 chars of distinct ASCII so chunks > 1.
	fp := &fakePartialPlatform{failAt: 2, maxLen: 8}
	s := &Scheduler{}
	s.configMapsPtr.Store(&cronConfigMaps{
		platforms: map[string]platform.Platform{"fake-notify": fp},
	})
	// 80 chars of ASCII -> SplitText with maxLen=8 yields 10 chunks. Even if
	// SplitText changes behaviour, we only need >= 4 chunks to make the
	// abort assertion meaningful (failAt=2 + abort means total Reply calls
	// stay at 3: indexes 0, 1, 2 where index 2 is the failing one).
	long := buildDistinctChunks(10, 8)
	s.notifyTarget("fake-notify", "chat-x", long)
	// uniqueChunks counts distinct chunk Texts handed to Reply. The loop
	// must visit failAt+1 unique chunks (indexes 0..failAt where failAt is
	// the first failing one) and stop there — anything more means the
	// partial-delivery regression is back.
	got := fp.uniqueChunks()
	if got != fp.failAt+1 {
		t.Errorf("unique chunks attempted = %d, want %d (loop must abort on first failure, not move on to remaining chunks)", got, fp.failAt+1)
	}
}

// TestR249CR26_NotifyTargetPartialMetric pins #966: the send-failure abort
// path bumps metrics.CronNotifyPartialTotal exactly once so operators can
// alert on a rising delta. expvar counters are process-global and other
// parallel tests in this package also drive partial deliveries, so we assert
// the counter advanced by at least one rather than an exact delta (the
// per-call increment is exercised exactly once below; concurrent tests can
// only push the observed delta higher, never below 1).
func TestR249CR26_NotifyTargetPartialMetric(t *testing.T) {
	before := metrics.CronNotifyPartialTotal.Value()
	fp := &fakePartialPlatform{failAt: 2, maxLen: 8}
	s := &Scheduler{}
	s.configMapsPtr.Store(&cronConfigMaps{
		platforms: map[string]platform.Platform{"fake-notify": fp},
	})
	long := buildDistinctChunks(10, 8)
	s.notifyTarget("fake-notify", "chat-x", long)
	if delta := metrics.CronNotifyPartialTotal.Value() - before; delta < 1 {
		t.Errorf("CronNotifyPartialTotal delta = %d, want >= 1 (partial delivery must bump counter)", delta)
	}
}

// TestR250CR18_NotifyTargetAllSucceedSendsAll verifies the happy path is
// untouched: when every Reply succeeds, every chunk is delivered.
//
// R236-SEC-15 (#568) introduced cronNotifyMaxChunks; this test uses an
// input that produces fewer chunks than the cap so the assertion still
// reads as "every chunk SplitText emits is delivered". The dedicated
// cap-truncation regression lives in notify_target_chunk_cap_test.go.
func TestR250CR18_NotifyTargetAllSucceedSendsAll(t *testing.T) {
	t.Parallel()
	// failAt larger than chunk count -> never fails.
	fp := &fakePartialPlatform{failAt: 1000, maxLen: 8}
	s := &Scheduler{}
	s.configMapsPtr.Store(&cronConfigMaps{
		platforms: map[string]platform.Platform{"fake-notify": fp},
	})
	// 3 chunks stays comfortably under cronNotifyMaxChunks (=5).
	long := buildDistinctChunks(3, 8)
	s.notifyTarget("fake-notify", "chat-x", long)
	// All chunks SplitText produces should have been Reply'd at least once.
	chunks := platform.SplitText(long, 8)
	if len(chunks) > cronNotifyMaxChunks {
		t.Fatalf("test fixture: chunk count %d exceeds cap %d; rewrite test to stay under cap", len(chunks), cronNotifyMaxChunks)
	}
	if got := fp.uniqueChunks(); got != len(chunks) {
		t.Errorf("unique chunks attempted = %d, want %d (success path must send every chunk)", got, len(chunks))
	}
}
