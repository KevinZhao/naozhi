package cron

import (
	"os"
	"strings"
	"testing"
)

// TestRegisterJob_UsesNewCronTickCallbackFactory pins R246-ARCH-9 (#785)
// narrow-scope progress: the registerJob → robfig/cron AddFunc dispatch
// boundary must route through the (*Scheduler).newCronTickCallback factory
// rather than an inline anonymous closure. Centralising the factory
// documents three otherwise easy-to-violate contracts in one place:
//
//  1. jobID-by-value capture (no *Job pointer leak past UpdateJob);
//  2. delegation to executeJobIDIfLive (shared paused/deleted pre-flight
//     gate with TriggerNow);
//  3. viaTriggerNow=false / logSubject="cron" pinned at the dispatch
//     boundary so trigger-source labels stay in lockstep.
//
// The Scheduler.stopCtx struct field is still required (robfig/cron's
// AddFunc takes a func() with no ctx parameter slot, so the field is the
// only way Stop() can cancel in-flight work — the anti-pattern called out
// by the issue is bounded by upstream API limits, not a fixable Go code
// smell). The factory is the single anchor a future ctx-aware AddFunc
// shim would attach to; this test guards that anchor's existence.
func TestRegisterJob_UsesNewCronTickCallbackFactory(t *testing.T) {
	src, err := os.ReadFile("scheduler_jobs.go")
	if err != nil {
		t.Fatalf("read scheduler_jobs.go: %v", err)
	}
	body := string(src)

	// 1. Factory must exist with the documented receiver/signature.
	const factorySig = "func (s *Scheduler) newCronTickCallback(jobID string) func()"
	if !strings.Contains(body, factorySig) {
		t.Fatalf("newCronTickCallback factory missing — R246-ARCH-9 / #785 " +
			"narrow-scope refactor reverted. The robfig/cron AddFunc " +
			"dispatch boundary must be encapsulated in one factory so the " +
			"jobID-capture / executeJobIDIfLive-delegation / trigger-source " +
			"label contracts live in one godoc'd place.")
	}

	// 2. registerJob must call the factory at the AddFunc site instead of
	// an inline closure.
	const fnMarker = "func (s *Scheduler) registerJob("
	idx := strings.Index(body, fnMarker)
	if idx < 0 {
		t.Fatalf("registerJob function not found in scheduler_jobs.go")
	}
	rest := body[idx:]
	if next := strings.Index(rest[len(fnMarker):], "\nfunc "); next >= 0 {
		rest = rest[:len(fnMarker)+next]
	}
	if !strings.Contains(rest, "s.cron.AddFunc(j.Schedule, s.newCronTickCallback(jobID))") {
		t.Error("registerJob must register the cron entry via " +
			"s.cron.AddFunc(j.Schedule, s.newCronTickCallback(jobID)) — " +
			"R246-ARCH-9 / #785. An inline closure here decentralises the " +
			"dispatch-boundary contract again.")
	}
	// Belt-and-braces: an inline `func() {` inside registerJob's body
	// would indicate the AddFunc call still wraps an anonymous closure.
	// Allow factory function literals elsewhere; just guard the AddFunc
	// site.
	if strings.Contains(rest, "AddFunc(j.Schedule, func()") {
		t.Error("registerJob: inline `func()` closure detected at AddFunc " +
			"site — must delegate to newCronTickCallback factory.")
	}

	// 3. Factory must call executeJobIDIfLive with the pinned trigger
	// label so a future maintainer cannot quietly switch to viaTriggerNow=true
	// or rename the log subject.
	idxFactory := strings.Index(body, factorySig)
	if idxFactory < 0 {
		t.Fatalf("factory signature search failed (already verified above)")
	}
	factoryRest := body[idxFactory:]
	if next := strings.Index(factoryRest[len(factorySig):], "\nfunc "); next >= 0 {
		factoryRest = factoryRest[:len(factorySig)+next]
	}
	if !strings.Contains(factoryRest, "s.executeJobIDIfLive(jobID, false") {
		t.Error("newCronTickCallback must invoke executeJobIDIfLive(jobID, false, ...) — " +
			"the shared paused/deleted gate with TriggerNow.")
	}
	if !strings.Contains(factoryRest, `"cron"`) {
		t.Error("newCronTickCallback must pass logSubject=\"cron\" so " +
			"shutdown-race traces remain distinguishable from TriggerNow.")
	}
}
