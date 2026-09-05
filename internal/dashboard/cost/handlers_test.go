package cost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
)

// reopen returns a fresh Store over the same directory so summaries read the
// flushed files (the seeding store is closed).
func seeded(t *testing.T) *costledger.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cost")
	now := time.Now()
	w := costledger.NewStore(dir, costledger.Options{})
	w.Append(costledger.Entry{TS: now.Add(-time.Hour), Source: costledger.SourceSession, Kind: costledger.KindTurn,
		Backend: "claude", Unit: costledger.UnitUSD, Amount: 1.5, Basis: costledger.BasisList, SessionKey: "feishu:p2p:u1"})
	w.Append(costledger.Entry{TS: now.Add(-2 * time.Hour), Source: costledger.SourceCronLocal, Kind: costledger.KindTurn,
		Backend: "claude", Unit: costledger.UnitUSD, Amount: 0.5, Basis: costledger.BasisList, JobID: "0123456789abcdef"})
	w.Append(costledger.Entry{TS: now.Add(-3 * time.Hour), Source: costledger.SourceSession, Kind: costledger.KindMetering,
		Backend: "kiro", Unit: costledger.UnitCredits, Amount: 4})
	w.Close()
	s := costledger.NewStore(dir, costledger.Options{})
	t.Cleanup(s.Close)
	return s
}

func get(t *testing.T, h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestSummary_DefaultWindowBucketsByUnit(t *testing.T) {
	h := New(Deps{Ledger: seeded(t)})
	rec := get(t, h.HandleSummary, "/api/cost/summary?group_by=source")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp summaryResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var usd, credits float64
	for _, b := range resp.Buckets {
		if b.Key == "session" && b.Unit == costledger.UnitUSD {
			usd = b.Amount
		}
		if b.Key == "session" && b.Unit == costledger.UnitCredits {
			credits = b.Amount
		}
	}
	if usd != 1.5 || credits != 4 {
		t.Fatalf("session buckets usd=%v credits=%v: %+v", usd, credits, resp.Buckets)
	}
	if resp.Note != "" {
		t.Fatalf("no drops, no note expected: %q", resp.Note)
	}
}

func TestSummary_RejectsBadParams(t *testing.T) {
	h := New(Deps{Ledger: seeded(t)})
	cases := []string{
		"/api/cost/summary?group_by=owner",
		"/api/cost/summary?from=yesterday",
		"/api/cost/summary?from=2026-09-05T00:00:00Z&to=2026-09-04T00:00:00Z",
		"/api/cost/summary?from=2026-01-01T00:00:00Z&to=2026-09-01T00:00:00Z",
		"/api/cost/summary?job_id=" + "%01bad",
		"/api/cost/summary?session_key=" + strings.Repeat("a", maxFilterLen+1),
	}
	for _, c := range cases {
		if rec := get(t, h.HandleSummary, c); rec.Code != 400 {
			t.Errorf("%s: status %d, want 400", c, rec.Code)
		}
	}
	ok := "/api/cost/summary?from=2026-01-01T00:00:00Z&to=2026-09-01T00:00:00Z&allow_full_range=1&group_by=day"
	if rec := get(t, h.HandleSummary, ok); rec.Code != 200 {
		t.Errorf("full range opt-in: status %d", rec.Code)
	}
	if rec := get(t, h.HandleSummary, "/api/cost/summary?from=2026-01-01T00:00:00Z&to=2026-09-01T00:00:00Z&allow_full_range=true"); rec.Code != 400 {
		t.Errorf("allow_full_range=true must not opt in: status %d", rec.Code)
	}
	if rec := get(t, h.HandleSummary, "/api/cost/summary?from=2026-01-01T00:00:00Z&to=2026-04-01T00:00:00Z"); rec.Code != 200 {
		t.Errorf("exactly 90 days must pass: status %d", rec.Code)
	}
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if rec := get(t, h.HandleSummary, "/api/cost/summary?to="+future); rec.Code != 200 {
		t.Errorf("future to must be accepted: status %d", rec.Code)
	}
	if rec := get(t, h.HandleSummary, "/api/cost/summary?job_id=0123456789abcdef&workspace=proj&group_by=job"); rec.Code != 200 {
		t.Errorf("combined filters: status %d", rec.Code)
	}
	bad := get(t, h.HandleSummary, "/api/cost/summary?group_by=owner")
	if ct := bad.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("errors must be JSON envelopes, got Content-Type %q", ct)
	}
}

func TestSummary_FilterByJob(t *testing.T) {
	h := New(Deps{Ledger: seeded(t)})
	rec := get(t, h.HandleSummary, "/api/cost/summary?group_by=job&job_id=0123456789abcdef")
	var resp summaryResp
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Buckets) != 1 || resp.Buckets[0].Amount != 0.5 {
		t.Fatalf("job buckets = %+v", resp.Buckets)
	}
}

func TestEntries_LimitClampedAndNewestFirst(t *testing.T) {
	h := New(Deps{Ledger: seeded(t)})
	rec := get(t, h.HandleEntries, "/api/cost/entries?limit=2")
	var resp entriesResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != 200 {
		t.Fatalf("status %d err %v", rec.Code, err)
	}
	if len(resp.Entries) != 2 || !resp.Entries[0].TS.After(resp.Entries[1].TS) {
		t.Fatalf("entries = %+v", resp.Entries)
	}
	if rec := get(t, h.HandleEntries, "/api/cost/entries?limit=0"); rec.Code != 400 {
		t.Errorf("limit=0 status %d", rec.Code)
	}
}

func TestGate_DisabledLedgerAndLimiter(t *testing.T) {
	var nilStore *costledger.Store
	h := New(Deps{Ledger: nilStore})
	if rec := get(t, h.HandleSummary, "/api/cost/summary"); rec.Code != 503 {
		t.Fatalf("nil ledger status %d", rec.Code)
	}
	h = New(Deps{Ledger: costledger.NewStore("", costledger.Options{})})
	if rec := get(t, h.HandleSummary, "/api/cost/summary"); rec.Code != 503 {
		t.Fatalf("disabled ledger status %d", rec.Code)
	}
	h = New(Deps{Ledger: seeded(t), Limiter: denyAll{}})
	if rec := get(t, h.HandleSummary, "/api/cost/summary"); rec.Code != 429 {
		t.Fatalf("limited status %d", rec.Code)
	}
	if !h.HasLimiter() {
		t.Fatal("HasLimiter must report the wired limiter")
	}
}

type denyAll struct{}

func (denyAll) Allow(string) bool               { return false }
func (denyAll) AllowRequest(*http.Request) bool { return false }
