// Package cliusage turns the CLI's per-model usage rows into ledger values.
// J1 of #2548.
//
// This ten-field mapping existed three times, field for field:
// internal/session/managed_cost.go, internal/sysession/runner_cost.go and
// internal/agentcore/envelope.go. Each also carried its own mirror of the wire
// struct — clievent.ModelUsage, sysession.resultModelUsage,
// agentcore.modelUsageRow — with identical field names and identical JSON tags.
//
// Three copies of a field mapping is three places to forget a field. A new usage
// dimension (thinkingTokens was one) lands in whichever copies the author
// happened to find, and the ones that were missed silently report zero rather
// than failing: the ledger still balances, the row is just wrong. None of the
// three call sites asserted token pass-through, so nothing would have caught it;
// the single mapping here is gated on every field.
//
// clievent.ModelUsage is the canonical shape — it is what the CLI actually emits
// and what the other two were mirroring — so the other two are gone.
//
// Why a package of its own rather than a function in costledger: costledger is a
// leaf by contract (docs/rfc/cost-ledger.md §4, pinned by its TestPackageIsLeaf)
// because every producer imports it. An adapter that knows both the wire shape
// and the ledger shape has to live outside that leaf, and putting it in clievent
// instead would point a data model at a store.
package cliusage

import (
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
)

// Cumulative builds a ledger Cumulative from a total USD figure and the CLI's
// per-model usage rows. Models is left nil when there are no rows, which
// costledger.Delta treats as "no per-model detail" rather than "all zero".
func Cumulative(usd float64, models map[string]clievent.ModelUsage) costledger.Cumulative {
	out := costledger.Cumulative{USD: usd}
	if len(models) == 0 {
		return out
	}
	out.Models = make(map[string]costledger.ModelUsage, len(models))
	for k, v := range models {
		out.Models[k] = costledger.ModelUsage{
			Tokens: costledger.Tokens{
				Input: v.InputTokens, Output: v.OutputTokens,
				CacheRead: v.CacheReadInputTokens, CacheWrite: v.CacheCreationInputTokens,
				Thinking: v.ThinkingTokens, WebSearch: v.WebSearchRequests,
			},
			CostUSD:   v.CostUSD,
			Canonical: v.CanonicalModel,
			Provider:  v.Provider,
			Basis:     costledger.Basis(v.CostBasis),
		}
	}
	return out
}
