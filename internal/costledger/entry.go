// Package costledger is the single append-only ledger every cost producer in
// naozhi writes to (session turns, cron local/sandbox runs, sysession runs)
// and the only place "what did this machine / job / model spend" is answered
// from. Leaf package: it must not import any internal/* package.
// See docs/rfc/cost-ledger.md.
package costledger

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Source names the run owner that wrote the entry.
type Source string

// Unit is the billing unit; buckets never sum across units.
type Unit string

// Kind is how the amount was collected.
type Kind string

// Basis is the CLI price table the amount was estimated against.
type Basis string

const (
	SourceSession     Source = "session"
	SourceCronLocal   Source = "cron_local"
	SourceCronSandbox Source = "cron_sandbox"
	SourceSysession   Source = "sysession"

	UnitUSD     Unit = "USD"
	UnitCredits Unit = "credits"
	UnitTokens  Unit = "tokens"

	KindTurn     Kind = "turn"     // CLI cumulative differenced per turn
	KindReceipt  Kind = "receipt"  // sandbox execution receipt
	KindMetering Kind = "metering" // backend metering rows (kiro/codex)
	KindBackfill Kind = "backfill"
	KindPartial  Kind = "partial" // turn ended without a result frame

	BasisList    Basis = "list"
	BasisManaged Basis = "managed"
	BasisUnknown Basis = "unknown"
	BasisNone    Basis = ""
)

// MaxModels bounds Entry.Models so a misbehaving CLI cannot inflate a line.
const MaxModels = 16

// maxIdentLen bounds model/provider strings copied from CLI output.
const maxIdentLen = 128

// invalidIdent replaces a model/provider string that failed validation.
const invalidIdent = "<invalid>"

// Entry is one billing event. Amount is the increment in Unit and the only
// authoritative figure; Models is a per-model drill-down that never feeds the
// rollup sums.
type Entry struct {
	TS         time.Time    `json:"ts"`
	Source     Source       `json:"source"`
	Kind       Kind         `json:"kind"`
	SessionKey string       `json:"session_key,omitempty"`
	JobID      string       `json:"job_id,omitempty"`
	RunID      string       `json:"run_id,omitempty"`
	Workspace  string       `json:"workspace,omitempty"`
	Backend    string       `json:"backend"`
	Unit       Unit         `json:"unit"`
	Amount     float64      `json:"amount"`
	Basis      Basis        `json:"basis,omitempty"`
	Models     []ModelDelta `json:"models,omitempty"`
}

// ModelDelta is one model's share of an Entry.
type ModelDelta struct {
	Model    string  `json:"model"`
	RawModel string  `json:"raw_model,omitempty"`
	Provider string  `json:"provider,omitempty"`
	Basis    Basis   `json:"basis,omitempty"`
	CostUSD  float64 `json:"cost_usd"`
	Tokens
}

// Tokens is the token breakdown carried on ModelDelta and summed in buckets.
type Tokens struct {
	Input      int64 `json:"input,omitempty"`
	Output     int64 `json:"output,omitempty"`
	CacheRead  int64 `json:"cache_read,omitempty"`
	CacheWrite int64 `json:"cache_write,omitempty"`
	Thinking   int64 `json:"thinking,omitempty"`
	WebSearch  int64 `json:"web_search,omitempty"`
}

func (t Tokens) add(o Tokens) Tokens {
	return Tokens{
		Input: t.Input + o.Input, Output: t.Output + o.Output,
		CacheRead: t.CacheRead + o.CacheRead, CacheWrite: t.CacheWrite + o.CacheWrite,
		Thinking: t.Thinking + o.Thinking, WebSearch: t.WebSearch + o.WebSearch,
	}
}

func (s Source) valid() bool {
	switch s {
	case SourceSession, SourceCronLocal, SourceCronSandbox, SourceSysession:
		return true
	}
	return false
}

func (u Unit) valid() bool {
	switch u {
	case UnitUSD, UnitCredits, UnitTokens:
		return true
	}
	return false
}

func (k Kind) valid() bool {
	switch k {
	case KindTurn, KindReceipt, KindMetering, KindBackfill, KindPartial:
		return true
	}
	return false
}

// normalizeBasis maps anything outside the CLI enum to BasisUnknown; empty
// stays empty (non-claude backends have no price table).
func normalizeBasis(b Basis) Basis {
	switch b {
	case BasisList, BasisManaged, BasisUnknown, BasisNone:
		return b
	}
	return BasisUnknown
}

// worseBasis orders unknown > managed > list > "" so a mixed turn reports the
// least trustworthy basis.
func worseBasis(a, b Basis) Basis {
	rank := func(x Basis) int {
		switch x {
		case BasisUnknown:
			return 3
		case BasisManaged:
			return 2
		case BasisList:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// sanitizeIdent validates a model/provider string copied from CLI output:
// non-empty, <= maxIdentLen bytes, valid UTF-8, no C0/DEL control bytes.
// Anything else becomes invalidIdent so the JSONL line and logs stay clean.
func sanitizeIdent(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > maxIdentLen || !utf8.ValidString(s) {
		return invalidIdent
	}
	if strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return invalidIdent
	}
	return s
}

// normalize validates enums, sanitizes CLI-sourced strings and caps Models.
// It returns false when the entry must be rejected outright: invalid
// Source/Unit/Kind, empty Backend, or nothing to record (Amount <= 0 and no
// Models).
func (e *Entry) normalize() bool {
	if !e.Source.valid() || !e.Unit.valid() || !e.Kind.valid() || e.Backend == "" {
		return false
	}
	if !(e.Amount > 0) && len(e.Models) == 0 {
		return false
	}
	if e.Amount < 0 {
		e.Amount = 0
	}
	e.Basis = normalizeBasis(e.Basis)
	e.Backend = sanitizeIdent(e.Backend)
	if len(e.Models) > MaxModels {
		e.Models = e.Models[:MaxModels]
	}
	for i := range e.Models {
		m := &e.Models[i]
		m.Model = sanitizeIdent(m.Model)
		m.RawModel = sanitizeIdent(m.RawModel)
		m.Provider = sanitizeIdent(m.Provider)
		m.Basis = normalizeBasis(m.Basis)
	}
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	e.TS = e.TS.UTC()
	return true
}
