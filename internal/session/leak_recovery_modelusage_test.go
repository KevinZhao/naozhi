package session

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestRecover_CopiesModelUsage pins docs/rfc/cost-ledger.md §5.1: the
// recovered result is rebuilt field by field, and the per-model cumulative
// snapshot must travel with CostUSD or the turn's model delta is lost.
func TestRecover_CopiesModelUsage(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	s, proc := newLeakSession(nil)
	recovered := &clievent.SendResult{
		Text:    "clean",
		CostUSD: 0.5,
		ModelUsage: map[string]clievent.ModelUsage{
			"m[1m]": {InputTokens: 10, CostUSD: 0.5, CostBasis: "list"},
		},
	}
	resend := func(context.Context, string) (*clievent.SendResult, error) { return recovered, nil }
	out := s.recoverLeakedToolcall(context.Background(), proc, &clievent.SendResult{Text: leakSample, CostUSD: 0.3}, resend)
	if out == nil || out.Text != "clean" {
		t.Fatalf("recovery did not fire: %+v", out)
	}
	if mu, ok := out.ModelUsage["m[1m]"]; !ok || mu.InputTokens != 10 || mu.CostUSD != 0.5 {
		t.Fatalf("ModelUsage not carried through recovery: %+v", out.ModelUsage)
	}
}
