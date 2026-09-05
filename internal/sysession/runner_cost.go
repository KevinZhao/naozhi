package sysession

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
)

// CostLedger is the sink daemon runs book their spend to; satisfied by
// *costledger.Store (nil receiver reports Enabled()==false).
type CostLedger interface {
	Enabled() bool
	Append(costledger.Entry) bool
}

// resultEnvelope is the subset of `claude -p --output-format json` this
// package reads: the reply text plus the cost receipt.
type resultEnvelope struct {
	Type       string                      `json:"type"`
	Subtype    string                      `json:"subtype"`
	IsError    bool                        `json:"is_error"`
	Result     string                      `json:"result"`
	CostUSD    float64                     `json:"total_cost_usd"`
	ModelUsage map[string]resultModelUsage `json:"modelUsage"`
}

type resultModelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	ThinkingTokens           int64   `json:"thinkingTokens"`
	WebSearchRequests        int64   `json:"webSearchRequests"`
	CostUSD                  float64 `json:"costUSD"`
	CanonicalModel           string  `json:"canonicalModel"`
	Provider                 string  `json:"provider"`
	CostBasis                string  `json:"costBasis"`
}

// parseResultEnvelope decodes the one-shot CLI's stdout. Truncated or
// non-JSON output is an error: the daemon must not receive JSON fragments as
// if they were its reply. An is_error envelope is returned WITH the error so
// the caller can still book the spend the failed run incurred.
func parseResultEnvelope(stdout []byte) (resultEnvelope, error) {
	var env resultEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &env); err != nil || env.Type != "result" {
		metrics.SysessionRunnerParseFailTotal.Add(1)
		return resultEnvelope{}, fmt.Errorf("sysession: claude -p returned no parsable json result (head: %s)",
			osutil.SanitizeForLog(string(stdout), 100))
	}
	if env.IsError {
		return env, fmt.Errorf("sysession: claude -p reported an error (%s): %s",
			env.Subtype, osutil.SanitizeForLog(env.Result, 256))
	}
	return env, nil
}

// bookRunCost appends the one-shot process's spend. A `-p` process lives
// for exactly one turn, so its cumulative reading is already the increment.
func (r *runnerImpl) bookRunCost(env resultEnvelope, ri RunInfo) {
	if r.cfg.Ledger == nil || !r.cfg.Ledger.Enabled() {
		return
	}
	raw := costledger.Cumulative{USD: env.CostUSD}
	if len(env.ModelUsage) > 0 {
		raw.Models = make(map[string]costledger.ModelUsage, len(env.ModelUsage))
		for k, v := range env.ModelUsage {
			raw.Models[k] = costledger.ModelUsage{
				Tokens: costledger.Tokens{
					Input: v.InputTokens, Output: v.OutputTokens,
					CacheRead: v.CacheReadInputTokens, CacheWrite: v.CacheCreationInputTokens,
					Thinking: v.ThinkingTokens, WebSearch: v.WebSearchRequests,
				},
				CostUSD: v.CostUSD, Canonical: v.CanonicalModel, Provider: v.Provider,
				Basis: costledger.Basis(v.CostBasis),
			}
		}
	}
	inc, _ := costledger.Delta(raw, costledger.Cumulative{})
	if inc.USD <= 0 && len(inc.Models) == 0 {
		return
	}
	basis := inc.Basis
	if basis == costledger.BasisNone {
		basis = costledger.BasisList
	}
	e := costledger.Entry{
		Source:     costledger.SourceSysession,
		Kind:       costledger.KindTurn,
		SessionKey: "sys:" + ri.Daemon,
		RunID:      ri.RunID,
		Workspace:  filepath.Base(r.cfg.WorkDir),
		Backend:    "claude",
		Unit:       costledger.UnitUSD,
		Amount:     inc.USD,
		Basis:      basis,
		Models:     inc.Models,
	}
	if !r.cfg.Ledger.Append(e) {
		slog.Warn("sysession: cost entry not recorded", "daemon", ri.Daemon, "run_id", ri.RunID)
	}
}
