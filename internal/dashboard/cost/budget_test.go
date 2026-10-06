package cost

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/budget"
	"github.com/naozhi/naozhi/internal/costledger"
)

// budgetNow is 03:00 on 6 Sep in UTC+8, still 5 Sep in UTC: the budget day
// is the operator's.
var (
	budgetLoc = time.FixedZone("UTC+8", 8*3600)
	budgetNow = time.Date(2026, 9, 6, 3, 0, 0, 0, budgetLoc)
)

func budgetGate(lim budget.Limits, spend map[[2]string]float64) *budget.Gate {
	idx := budget.NewIndex(budgetLoc, func() time.Time { return budgetNow })
	for kj, amt := range spend {
		idx.Add(costledger.Entry{TS: budgetNow, Unit: costledger.UnitUSD, Amount: amt, SessionKey: kj[0], JobID: kj[1]})
	}
	return budget.NewGate(lim, idx)
}

func getBudget(t *testing.T, h *Handlers, query string) budgetResp {
	t.Helper()
	rec := get(t, h.HandleBudget, "/api/cost/budget"+query)
	if rec.Code != 200 {
		t.Fatalf("%s: status %d: %s", query, rec.Code, rec.Body)
	}
	var resp budgetResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// Without cost.budget the endpoint answers, but says no cap is configured.
func TestBudget_NoGate(t *testing.T) {
	h := New(Deps{Ledger: seeded(t)})
	if got := getBudget(t, h, "?session_key=feishu:group:oc_1:general"); got != (budgetResp{}) {
		t.Fatalf("no gate = %+v, want the zero answer", got)
	}
}

// Each query reports the cap the IM or cron gate would check: the scoped one
// or the machine-wide one, whichever is nearer its limit.
func TestBudget_ReportsTheNearestCap(t *testing.T) {
	h := New(Deps{Budget: budgetGate(
		budget.Limits{PerChatDailyUSD: 5, PerJobDailyUSD: 2, DailyUSD: 20},
		map[[2]string]float64{
			{"feishu:group:oc_1:general", ""}:  4.5,
			{"feishu:group:oc_1:reviewer", ""}: 1,
			{"", "job1"}:                       1.75,
			{"dashboard:direct:x:general", ""}: 8,
		})})
	reset := time.Date(2026, 9, 7, 0, 0, 0, 0, budgetLoc)
	cases := []struct {
		query string
		want  budgetResp
	}{
		{"?session_key=feishu:group:oc_1:general", budgetResp{Enabled: true, Scope: "chat", Subject: "feishu:group:oc_1",
			Spent: 5.5, Limit: 5, Warn: true, Over: true, Blocked: true, Day: "2026-09-06", ResetAt: reset}},
		{"?job_id=job1", budgetResp{Enabled: true, Scope: "job", Subject: "job1",
			Spent: 1.75, Limit: 2, Warn: true, Day: "2026-09-06", ResetAt: reset}},
		{"?session_key=feishu:group:oc_2:general", budgetResp{Enabled: true, Scope: "global",
			Spent: 15.25, Limit: 20, Day: "2026-09-06", ResetAt: reset}},
		{"", budgetResp{Enabled: true, Scope: "global", Spent: 15.25, Limit: 20, Day: "2026-09-06", ResetAt: reset}},
	}
	for _, c := range cases {
		got := getBudget(t, h, c.query)
		if !got.ResetAt.Equal(c.want.ResetAt) {
			t.Errorf("%s: reset_at %v, want %v", c.query, got.ResetAt, c.want.ResetAt)
		}
		got.ResetAt, c.want.ResetAt = time.Time{}, time.Time{}
		if got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.query, got, c.want)
		}
	}
}

// A key no configured cap covers gets enabled with no scope, so the chip
// stays hidden; action warn never reports blocked.
func TestBudget_NoApplicableCapAndWarnAction(t *testing.T) {
	h := New(Deps{Budget: budgetGate(budget.Limits{PerChatDailyUSD: 1, Action: budget.ActionWarn},
		map[[2]string]float64{{"feishu:direct:u1:general", ""}: 3})})
	if got := getBudget(t, h, "?session_key=dashboard:direct:x:general"); got != (budgetResp{Enabled: true}) {
		t.Errorf("dashboard key = %+v, want enabled with no cap", got)
	}
	got := getBudget(t, h, "?session_key=feishu:direct:u1:general")
	if !got.Over || got.Blocked {
		t.Errorf("warn action = %+v, want over and not blocked", got)
	}
}

func TestBudget_RejectsBadParamsAndLimits(t *testing.T) {
	h := New(Deps{Budget: budgetGate(budget.Limits{DailyUSD: 1}, nil)})
	for _, q := range []string{
		"?session_key=feishu:group:oc_1:general&job_id=job1",
		"?job_id=%01bad",
		"?session_key=" + strings.Repeat("a", maxFilterLen+1),
	} {
		if rec := get(t, h.HandleBudget, "/api/cost/budget"+q); rec.Code != 400 {
			t.Errorf("%q: status %d, want 400", q, rec.Code)
		}
	}
	h = New(Deps{Budget: budgetGate(budget.Limits{DailyUSD: 1}, nil), Limiter: denyAll{}})
	if rec := get(t, h.HandleBudget, "/api/cost/budget"); rec.Code != 429 {
		t.Fatalf("limited status %d", rec.Code)
	}
}
