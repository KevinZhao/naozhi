// Package cost serves the dashboard's read-only view of the cost ledger
// (docs/rfc/cost-ledger.md §7): unit-bucketed summaries, audit entries and
// today's spend against cost.budget.
package cost

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/budget"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/dashboard/contracts"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/osutil"
)

// Ledger is the read side of *costledger.Store the handlers need.
type Ledger interface {
	Enabled() bool
	Summarize(costledger.Query) (costledger.Summary, error)
	Entries(costledger.Query, int) ([]costledger.Entry, error)
}

// Deps wires the handlers. Limiter nil = unlimited (tests only); Budget nil
// = cost.budget sets no cap.
type Deps struct {
	Ledger  Ledger
	Limiter contracts.IPLimiter
	Budget  *budget.Gate
}

// Handlers serves /api/cost/*.
type Handlers struct {
	ledger  Ledger
	limiter contracts.IPLimiter
	budget  *budget.Gate
}

func New(d Deps) *Handlers { return &Handlers{ledger: d.Ledger, limiter: d.Limiter, budget: d.Budget} }

// HasLimiter reports whether a rate limiter is wired (boot-time guard).
func (h *Handlers) HasLimiter() bool { return h.limiter != nil }

const (
	defaultWindow    = 30 * 24 * time.Hour
	maxFilterLen     = 256
	defaultEntries   = 200
	maxEntries       = 1000
	underReportNote  = "amount may be underestimated: ledger dropped entries"
	disabledErrorMsg = "cost ledger disabled"
)

type summaryResp struct {
	costledger.Summary
	Note string `json:"note,omitempty"`
}

type entriesResp struct {
	Entries []costledger.Entry `json:"entries"`
	Dropped int64              `json:"dropped"`
}

// budgetResp is today's spend against the cap nearest its limit. Scope is ""
// (and the rest zero) when no cap applies to the subject asked about.
type budgetResp struct {
	Enabled bool      `json:"enabled"`
	Scope   string    `json:"scope,omitempty"`
	Subject string    `json:"subject,omitempty"`
	Spent   float64   `json:"spent"`
	Limit   float64   `json:"limit"`
	Warn    bool      `json:"warn"`
	Over    bool      `json:"over"`
	Blocked bool      `json:"blocked"`
	Day     string    `json:"day,omitempty"`
	ResetAt time.Time `json:"reset_at,omitzero"`
}

// HandleSummary serves GET /api/cost/summary?from=&to=&group_by=&session_key=&job_id=&run_id=&workspace=&allow_full_range=1
func (h *Handlers) HandleSummary(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	q, err := parseQuery(r, costledger.GroupBySource)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sum, err := h.ledger.Summarize(q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid query window")
		return
	}
	resp := summaryResp{Summary: sum}
	if sum.Dropped > 0 {
		resp.Note = underReportNote
	}
	httputil.WriteJSON(w, resp)
}

// HandleEntries serves GET /api/cost/entries?...&limit= (newest first).
func (h *Handlers) HandleEntries(w http.ResponseWriter, r *http.Request) {
	if !h.gate(w, r) {
		return
	}
	q, err := parseQuery(r, costledger.GroupByDay)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := defaultEntries
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, maxEntries)
	}
	ents, err := h.ledger.Entries(q, limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid query window")
		return
	}
	var dropped int64
	if s, ok := h.ledger.(interface{ Dropped() int64 }); ok {
		dropped = s.Dropped()
	}
	httputil.WriteJSON(w, entriesResp{Entries: ents, Dropped: dropped})
}

// HandleBudget serves GET /api/cost/budget?session_key=|job_id=: what IM
// dispatch or the cron scheduler would check for that key or job (its chat,
// project or job cap, and the machine-wide one), or the machine-wide cap
// alone with neither. Read-only: the dashboard itself is never refused.
func (h *Handlers) HandleBudget(w http.ResponseWriter, r *http.Request) {
	if h.limiter != nil && !h.limiter.AllowRequest(r) {
		writeErr(w, http.StatusTooManyRequests, "rate limited")
		return
	}
	v := r.URL.Query()
	key, job := v.Get("session_key"), v.Get("job_id")
	if key != "" && job != "" {
		writeErr(w, http.StatusBadRequest, "session_key and job_id are exclusive")
		return
	}
	for name, s := range map[string]string{"session_key": key, "job_id": job} {
		if err := validateIdent(name, s); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if h.budget == nil {
		httputil.WriteJSON(w, budgetResp{})
		return
	}
	vd := h.budget.CheckKey(key)
	if job != "" {
		vd = h.budget.CheckJob(job)
	}
	resp := budgetResp{Enabled: true}
	if vd.Limit > 0 {
		resp = budgetResp{Enabled: true, Scope: vd.Subject.Kind(), Subject: vd.Subject.Name(),
			Spent: vd.Spent, Limit: vd.Limit, Warn: vd.Warn, Over: vd.Over, Blocked: vd.Blocked,
			Day: vd.ResetAt.AddDate(0, 0, -1).Format(time.DateOnly), ResetAt: vd.ResetAt}
	}
	httputil.WriteJSON(w, resp)
}

// gate applies the rate limit and the enabled check; false means a response
// was already written.
func (h *Handlers) gate(w http.ResponseWriter, r *http.Request) bool {
	if h.limiter != nil && !h.limiter.AllowRequest(r) {
		writeErr(w, http.StatusTooManyRequests, "rate limited")
		return false
	}
	if h.ledger == nil || !h.ledger.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, disabledErrorMsg)
		return false
	}
	return true
}

// writeErr keeps every error on this API in the same JSON envelope the other
// dashboard handlers use.
func writeErr(w http.ResponseWriter, status int, msg string) {
	httputil.WriteJSONStatus(w, status, map[string]string{"error": msg})
}

// parseQuery validates every user-controlled parameter before it reaches the
// ledger: RFC3339 window (default last 30 days), group_by whitelist, and
// filter identifiers bounded in length and character set.
func parseQuery(r *http.Request, defaultGroup costledger.GroupBy) (costledger.Query, error) {
	v := r.URL.Query()
	q := costledger.Query{GroupBy: defaultGroup}
	if g := v.Get("group_by"); g != "" {
		q.GroupBy = costledger.GroupBy(g)
	}
	if !validGroupBy(q.GroupBy) {
		return q, errors.New("unknown group_by")
	}
	now := time.Now()
	var err error
	if q.To, err = parseTime(v.Get("to"), now); err != nil {
		return q, fmt.Errorf("to: %w", err)
	}
	if q.From, err = parseTime(v.Get("from"), q.To.Add(-defaultWindow)); err != nil {
		return q, fmt.Errorf("from: %w", err)
	}
	if !q.To.After(q.From) {
		return q, errors.New("to must be after from")
	}
	// Only the literal "1" opts in; any other value keeps the 90-day cap.
	q.AllowFullRange = v.Get("allow_full_range") == "1"
	for name, dst := range map[string]*string{
		"session_key": &q.SessionKey, "job_id": &q.JobID, "run_id": &q.RunID, "workspace": &q.Workspace,
	} {
		s := v.Get(name)
		if s == "" {
			continue
		}
		if err := validateIdent(name, s); err != nil {
			return q, err
		}
		*dst = s
	}
	return q, nil
}

func parseTime(raw string, def time.Time) (time.Time, error) {
	if raw == "" {
		return def, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errors.New("must be RFC3339")
	}
	return t, nil
}

func validGroupBy(g costledger.GroupBy) bool {
	switch g {
	case costledger.GroupBySource, costledger.GroupByBackend, costledger.GroupByModel, costledger.GroupByJob,
		costledger.GroupBySession, costledger.GroupByWorkspace, costledger.GroupByDay, costledger.GroupByBasis,
		costledger.GroupByKind, costledger.GroupByUnit:
		return true
	}
	return false
}

// validateIdent bounds a filter value: <= maxFilterLen bytes, valid UTF-8, no
// C0/DEL control bytes, no log-injection runes.
func validateIdent(name, s string) error {
	if len(s) > maxFilterLen {
		return fmt.Errorf("%s too long", name)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s contains invalid characters", name)
	}
	if strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || osutil.IsLogInjectionRune(r) }) >= 0 {
		return fmt.Errorf("%s contains invalid characters", name)
	}
	return nil
}
