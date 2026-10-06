package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
)

func TestLedgerSpendUSD_SumsOnlyUSDForTheChat(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := costledger.NewStore(filepath.Join(t.TempDir(), "cost"), costledger.Options{Now: func() time.Time { return now }})
	if !s.Enabled() {
		t.Fatal("store disabled")
	}
	ents := []costledger.Entry{
		{TS: now.Add(-time.Hour), Source: costledger.SourceSession, Kind: costledger.KindTurn, SessionKey: "feishu:direct:u1:general", Backend: "claude", Unit: costledger.UnitUSD, Amount: 1.25},
		{TS: now.Add(-time.Hour), Source: costledger.SourceSession, Kind: costledger.KindTurn, SessionKey: "feishu:direct:u1:code-reviewer", Backend: "claude", Unit: costledger.UnitUSD, Amount: 0.75},
		{TS: now.Add(-time.Hour), Source: costledger.SourceSession, Kind: costledger.KindMetering, SessionKey: "feishu:direct:u1:general", Backend: "kiro", Unit: costledger.UnitCredits, Amount: 40},
		{TS: now.Add(-time.Hour), Source: costledger.SourceSession, Kind: costledger.KindTurn, SessionKey: "feishu:direct:u12:general", Backend: "claude", Unit: costledger.UnitUSD, Amount: 9},
		{TS: now.Add(-30 * time.Hour), Source: costledger.SourceSession, Kind: costledger.KindTurn, SessionKey: "feishu:direct:u1:general", Backend: "claude", Unit: costledger.UnitUSD, Amount: 100},
	}
	for i, e := range ents {
		if !s.Append(e) {
			t.Fatalf("append %d rejected", i)
		}
	}
	s.Close()

	got, err := ledgerSpendUSD(s)("feishu:direct:u1", now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2.0 {
		t.Fatalf("spend = %v, want 2.0 (two USD turns in window; credits, other chat and old entry excluded)", got)
	}
}

func TestBuildUserLimiter(t *testing.T) {
	t.Parallel()
	if buildUserLimiter(IMLimitsOptions{}) != nil {
		t.Fatal("zero options must disable the limiter")
	}
	l := buildUserLimiter(IMLimitsOptions{UserRatePerMinute: 60, UserRateBurst: 2})
	if l == nil || !l.Allow("k") || !l.Allow("k") || l.Allow("k") {
		t.Fatal("burst of 2 should admit exactly two immediate calls")
	}
}
