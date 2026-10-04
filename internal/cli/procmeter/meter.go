// Package procmeter holds what a running CLI process has reported about
// itself: cost, context usage, turn duration, the effort tier, the model and
// binary version, backend metering rows, and the shadow token account for a
// turn that ends without a result. The read loop is its writer; dashboard
// snapshots read it concurrently, mostly lock-free.
//
// Fields are private, so every write goes through a Meter method and keeps
// the invariants stated there (non-zero guards, change gates, the metering
// length published with the rows).
package procmeter

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// maxMeteringUnits bounds the metering rows so a buggy upstream inventing a
// unit per frame cannot grow them without limit.
const maxMeteringUnits = 16

// maxShadowModels bounds the shadow account's model rows; usage of any
// further model folds into one row with no model name.
const maxShadowModels = 16

// maxShadowMessages bounds the message ids the shadow account remembers for
// de-duplication. Past it the memory restarts: only a message whose frames
// straddle the restart can be counted twice.
const maxShadowMessages = 1024

// Meter is a process's self-reported state. The zero value is ready to use.
type Meter struct {
	totalCost           atomic.Uint64 // math.Float64bits of the last result's cost
	contextUsagePercent atomic.Uint64 // math.Float64bits
	turnDurationMs      atomic.Int64
	effort              atomic.Pointer[string]
	model               atomic.Pointer[string]
	liveVersion         atomic.Pointer[string]

	shadowMu sync.Mutex
	shadow   []clievent.ShadowModel
	// shadowMsgs is the usage last seen per API message id this turn.
	shadowMsgs      map[string]shadowMsg
	shadowLastModel string
	// lastResultMS is the read-loop receive time (unix ms) of the last
	// result frame; 0 before one.
	lastResultMS atomic.Int64

	meteringMu sync.RWMutex
	metering   []clievent.MeteringEntry
	// meteringIdx maps a unit to its row in metering; built on the first
	// metering frame so sessions that never report one never allocate it.
	meteringIdx map[string]int
	// meteringLen is len(metering), stored under meteringMu, so readers of
	// the dominant no-metering case skip the lock.
	meteringLen atomic.Int32
	// meteringGen counts metering writes; rows are unchanged while it is.
	meteringGen atomic.Uint64
}

// RecordResultCost stores the cost a result frame reports.
func (m *Meter) RecordResultCost(usd float64) {
	m.totalCost.Store(math.Float64bits(usd))
}

// TotalCost is the cost the last result frame reported. Lock-free.
func (m *Meter) TotalCost() float64 {
	return math.Float64frombits(m.totalCost.Load())
}

// ContextUsagePercent is the last reported context-window utilisation
// (0-100); 0 for backends that don't report it (claude stream-json). Lock-free.
func (m *Meter) ContextUsagePercent() float64 {
	return math.Float64frombits(m.contextUsagePercent.Load())
}

// TurnDurationMs is the duration of the most recently completed turn, in ms;
// 0 before any turn completes. Lock-free.
func (m *Meter) TurnDurationMs() int64 {
	return m.turnDurationMs.Load()
}

// Effort is the thinking-effort tier in force: the backend-reported one, or
// the spawn pin until the backend reports one; "" when neither is known.
// Lock-free.
func (m *Meter) Effort() string {
	return loadString(&m.effort)
}

// SeedEffort pre-fills the effort tier from the spawn pin. Fill-if-unset, so
// a backend-reported tier (possibly already replayed on reconnect) is never
// clobbered by the static pin.
func (m *Meter) SeedEffort(tier string) {
	if tier == "" {
		return
	}
	m.effort.CompareAndSwap(nil, &tier)
}

// Model is the CLI's model identifier, "" when unknown. Lock-free.
func (m *Meter) Model() string {
	return loadString(&m.model)
}

// SetModel records the model the CLI runs: the spawn pin, the one system/init
// resolves, or the one a set_model ack switched to. "" clears it.
func (m *Meter) SetModel(model string) {
	if model == "" {
		m.model.Store(nil)
		return
	}
	m.model.Store(&model)
}

// LiveVersion is the CLI binary version the process self-reported, "" before
// its init frame. Lock-free.
func (m *Meter) LiveVersion() string {
	return loadString(&m.liveVersion)
}

// SetLiveVersion records the self-reported binary version and reports whether
// it changed, so the caller fires its change hook once for the duplicate init
// captures (read loop + Send).
func (m *Meter) SetLiveVersion(v string) (changed bool) {
	if v == "" {
		return false
	}
	if prev := m.liveVersion.Load(); prev != nil && *prev == v {
		return false
	}
	m.liveVersion.Store(&v)
	return true
}

// ApplyMetadata stores a normalized metadata frame: scalars atomically,
// metering rows merged under the metering lock. Every field is guarded on
// being non-zero, so a frame that omits a field never regresses an earlier
// value.
func (m *Meter) ApplyMetadata(md *clievent.EventMetadata) {
	if md == nil {
		return
	}
	if md.ContextUsagePercent > 0 {
		m.contextUsagePercent.Store(math.Float64bits(md.ContextUsagePercent))
	}
	if md.TurnDurationMs > 0 {
		m.turnDurationMs.Store(md.TurnDurationMs)
	}
	// Overwrite semantics (unlike the per-unit accumulation below): effort is
	// a current-state tier, so xhigh→max mid-session must replace. The
	// change-gate avoids an allocation and a dirtied cache line per frame on
	// the value the 1 Hz × N-tab snapshot poll reads.
	if md.Effort != "" {
		if prev := m.effort.Load(); prev == nil || *prev != md.Effort {
			e := md.Effort
			m.effort.Store(&e)
		}
	}
	if len(md.MeteringUsage) > 0 {
		m.mergeMetering(md.MeteringUsage)
	}
}

// mergeMetering sums per-turn metering increments by unit: kiro reports
// increments and no running total.
func (m *Meter) mergeMetering(in []clievent.MeteringEntry) {
	m.meteringMu.Lock()
	defer m.meteringMu.Unlock()
	if m.meteringIdx == nil {
		m.meteringIdx = make(map[string]int, maxMeteringUnits)
		for i := range m.metering {
			m.meteringIdx[m.metering[i].Unit] = i
		}
	}
	for _, e := range in {
		if i, ok := m.meteringIdx[e.Unit]; ok {
			m.metering[i].Value += e.Value
			if e.UnitPlural != "" {
				m.metering[i].UnitPlural = e.UnitPlural
			}
			continue
		}
		if len(m.metering) >= maxMeteringUnits {
			continue
		}
		m.meteringIdx[e.Unit] = len(m.metering)
		m.metering = append(m.metering, e)
	}
	// Published under the lock so the lock-free fast path in Metering sees
	// the length the rows have once the lock is released.
	m.meteringLen.Store(int32(len(m.metering)))
	m.meteringGen.Add(1)
}

// Metering returns a copy of the session-total metering rows; nil for
// backends that report cost only through TotalCost (claude).
func (m *Meter) Metering() []clievent.MeteringEntry {
	if m.meteringLen.Load() == 0 {
		return nil
	}
	m.meteringMu.RLock()
	defer m.meteringMu.RUnlock()
	if len(m.metering) == 0 {
		return nil
	}
	out := make([]clievent.MeteringEntry, len(m.metering))
	copy(out, m.metering)
	return out
}

// MeteringGen is the number of metering writes so far: rows Metering returns
// are unchanged while it is, so pollers can cache the copy. Wait-free.
func (m *Meter) MeteringGen() uint64 {
	return m.meteringGen.Load()
}

// shadowMsg is one API message's usage as its latest frame reported it.
type shadowMsg struct {
	model string
	tok   clievent.ShadowModel
}

// TrackShadow folds an assistant frame's usage into the shadow account and
// clears it on the result frame, whose modelUsage supersedes it; nowMS is the
// read loop's receive time, kept as LastResultAt for a result. Frames sharing
// a message id count once, at the largest value each field reached.
func (m *Meter) TrackShadow(ev clievent.Event, nowMS int64) {
	switch ev.Type {
	case "assistant":
		if ev.Message == nil || ev.Message.Usage == nil {
			return
		}
		u := ev.Message.Usage
		tok := clievent.ShadowModel{Input: u.InputTokens, Output: u.OutputTokens,
			CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens}
		m.shadowMu.Lock()
		defer m.shadowMu.Unlock()
		model := ev.Message.Model
		if model == "" {
			model = m.shadowLastModel
		} else {
			m.shadowLastModel = model
		}
		if id := ev.Message.ID; id != "" {
			if prev, ok := m.shadowMsgs[id]; ok {
				next := maxTokens(prev.tok, tok)
				m.addShadow(prev.model, subTokens(next, prev.tok))
				m.shadowMsgs[id] = shadowMsg{model: prev.model, tok: next}
				return
			}
			if m.shadowMsgs == nil || len(m.shadowMsgs) >= maxShadowMessages {
				m.shadowMsgs = make(map[string]shadowMsg)
			}
			m.shadowMsgs[id] = shadowMsg{model: model, tok: tok}
		}
		m.addShadow(model, tok)
	case "result":
		m.lastResultMS.Store(nowMS)
		m.shadowMu.Lock()
		m.resetShadowLocked()
		m.shadowMu.Unlock()
	}
}

// addShadow adds tok to model's row. Caller holds shadowMu.
func (m *Meter) addShadow(model string, tok clievent.ShadowModel) {
	r := m.shadowRow(model)
	r.Input += tok.Input
	r.Output += tok.Output
	r.CacheRead += tok.CacheRead
	r.CacheWrite += tok.CacheWrite
}

// shadowRow returns model's row, adding it while under maxShadowModels and
// using the unnamed row past it. Caller holds shadowMu.
func (m *Meter) shadowRow(model string) *clievent.ShadowModel {
	for i := range m.shadow {
		if m.shadow[i].Model == model {
			return &m.shadow[i]
		}
	}
	if model != "" && len(m.shadow) >= maxShadowModels {
		return m.shadowRow("")
	}
	m.shadow = append(m.shadow, clievent.ShadowModel{Model: model})
	return &m.shadow[len(m.shadow)-1]
}

// resetShadowLocked empties the account. Caller holds shadowMu.
func (m *Meter) resetShadowLocked() {
	m.shadow = nil
	m.shadowMsgs = nil
	m.shadowLastModel = ""
}

func maxTokens(a, b clievent.ShadowModel) clievent.ShadowModel {
	return clievent.ShadowModel{Input: max(a.Input, b.Input), Output: max(a.Output, b.Output),
		CacheRead: max(a.CacheRead, b.CacheRead), CacheWrite: max(a.CacheWrite, b.CacheWrite)}
}

func subTokens(a, b clievent.ShadowModel) clievent.ShadowModel {
	return clievent.ShadowModel{Input: a.Input - b.Input, Output: a.Output - b.Output,
		CacheRead: a.CacheRead - b.CacheRead, CacheWrite: a.CacheWrite - b.CacheWrite}
}

// TakeShadow returns and clears the tokens consumed since the last result
// frame, for a turn that ends without one (death / timeout kill).
func (m *Meter) TakeShadow() clievent.ShadowUsage {
	m.shadowMu.Lock()
	u := clievent.ShadowUsage{Models: m.shadow}
	m.resetShadowLocked()
	m.shadowMu.Unlock()
	return u
}

// LastResultAt is when the read loop received the last result frame; zero
// before one. Lock-free.
func (m *Meter) LastResultAt() time.Time {
	if ms := m.lastResultMS.Load(); ms != 0 {
		return time.UnixMilli(ms)
	}
	return time.Time{}
}

func loadString(p *atomic.Pointer[string]) string {
	if s := p.Load(); s != nil {
		return *s
	}
	return ""
}
