package server

import (
	"os"
	"regexp"
	"testing"
)

// TestResubscribeTimeout_MarksSubGenReleasable pins #2224: the
// resubscribe-timeout cleanup arm in Hub.resubscribeEvents MUST mark
// subGen[key] for delayed reclamation (markSubGenReleasable +
// sweepSubGenExpiredLocked), exactly like handleUnsubscribe.
//
// Without it, the timeout path deleted c.subscriptions[key] and decremented
// the subscriber count but left subGen[key] pinned for the whole connection
// lifetime — sweepSubGenExpiredLocked only scans keys present in
// subGenReleaseAt, so an unmarked subGen entry is never reclaimed. A
// dashboard client that repeatedly subscribes to panels whose process is
// gone (resubscribe times out) would grow c.subGen without bound.
//
// This is a source-level anchor (the timeout arm is inline in
// resubscribeEvents and its real trigger is a 60s const window, not test-
// shortenable), matching TestSubGenReclaim_SourceAnchor / the H8 lock-order
// anchor in resubscribe_lock_order_test.go.
func TestResubscribeTimeout_MarksSubGenReleasable(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("wshub_eventpush.go")
	if err != nil {
		t.Fatalf("read wshub_eventpush.go: %v", err)
	}
	text := string(src)

	// Isolate the resubscribeEvents body so the anchor cannot be satisfied by
	// an unrelated occurrence elsewhere in the file (eventPushLoop etc.).
	bodyRe := regexp.MustCompile(`(?s)func \(h \*Hub\) resubscribeEvents\(.*?\n\}\n`)
	body := bodyRe.Find(src)
	if body == nil {
		t.Fatal("could not locate resubscribeEvents body")
	}

	// The timeout cleanup arm (the one that deletes the subscription on
	// subscription_timeout) must mark + sweep subGen.
	for _, want := range []string{
		"markSubGenReleasable(key",
		"sweepSubGenExpiredLocked",
		"#2224",
	} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).Match(body) {
			t.Errorf("resubscribeEvents timeout cleanup missing anchor %q — "+
				"#2224 subGen reclamation wiring removed? subGen[key] would leak "+
				"for the whole connection lifetime.", want)
		}
	}

	// Defensive ordering check: the mark/sweep must sit in the staleUnsub
	// cleanup block (between delete(c.subscriptions, key) and h.mu.Unlock()),
	// i.e. under h.mu where c.subGen is serialised.
	cleanupRe := regexp.MustCompile(`(?s)delete\(c\.subscriptions,\s*key\)\s*\n.*?markSubGenReleasable\(key.*?sweepSubGenExpiredLocked.*?h\.mu\.Unlock\(\)`)
	if !cleanupRe.MatchString(text) {
		t.Error("#2224: markSubGenReleasable/sweepSubGenExpiredLocked must run " +
			"inside the timeout cleanup block under h.mu (after " +
			"delete(c.subscriptions, key), before h.mu.Unlock()).")
	}
}
