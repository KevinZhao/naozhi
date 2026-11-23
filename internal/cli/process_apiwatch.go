package cli

import (
	"time"

	"github.com/naozhi/naozhi/internal/apiwatch"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// apiWatchSink is where a process books what its CLI said about its model API
// calls; Wrapper.APIWatch, nil for none.
type apiWatchSink struct {
	store   *apiwatch.Store
	key     string
	backend string
}

// setAPIWatch binds the store this process records to, under its session key.
// Called once before startReadLoop.
func (p *Process) setAPIWatch(store *apiwatch.Store, key, backend string) {
	if store == nil {
		return
	}
	p.apiWatch = &apiWatchSink{store: store, key: key, backend: backend}
}

// observeAPI books an api_retry frame or a result frame. The result's text is
// kept only when the turn failed: it is then the API's error message.
func (p *Process) observeAPI(ev clievent.Event, now time.Time) {
	w := p.apiWatch
	if w == nil {
		return
	}
	base := apiwatch.Record{At: now, Key: w.key, Backend: w.backend, Model: p.Model(), UUID: ev.UUID}
	switch {
	case ev.Type == "system" && ev.APIRetry != nil:
		r := ev.APIRetry
		base.Kind = apiwatch.KindRetry
		base.Attempt, base.MaxRetries, base.RetryDelayMS = r.Attempt, r.MaxRetries, r.RetryDelayMS
		base.Status, base.Category, base.WaitedMS = r.Status, r.Category, r.WaitedMS
	case ev.Type == "result":
		base.Kind = apiwatch.KindTurn
		if t := ev.APITiming; t != nil {
			base.DurationMS, base.APIMS, base.TTFTMS, base.Status = t.DurationMS, t.APIMS, t.TTFTMS, t.ErrorStatus
		}
		// A turn naozhi or the user stopped is not a failed API call.
		if ev.IsError && !ev.Aborted {
			base.Failed = true
			base.Error = ev.Result
		}
	default:
		return
	}
	w.store.Record(base)
}

// observeWatchdog books a turn the watchdog killed.
func (p *Process) observeWatchdog(reason string) {
	w := p.apiWatch
	if w == nil || (reason != DeathReasonNoOutputTimeout && reason != DeathReasonTotalTimeout) {
		return
	}
	w.store.Record(apiwatch.Record{Kind: apiwatch.KindTimeout, Key: w.key, Backend: w.backend,
		Model: p.Model(), Reason: reason})
}
