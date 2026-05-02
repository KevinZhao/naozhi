// Package claudejsonl implements history.Source on top of the Claude Code
// CLI's per-session JSONL transcripts under ~/.claude/projects/.
//
// Each session key may span several Claude session IDs (a "chain") — the
// CLI rotates IDs on /new, --resume, or workspace switches, and naozhi
// tracks the chain in ManagedSession.prevSessionIDs. LoadBefore walks the
// chain newest → oldest via discovery.LoadHistoryChainBeforeCtx, which
// handles the reverse JSONL tail-read and the strictly-less-than filter.
//
// The chain is supplied through a callback rather than a snapshot: the
// session can mutate its chain (new /new, resume, workspace change) while
// a pagination request is in flight, and we want the next page to see
// the latest chain. The callback is expected to return a consistent
// oldest → newest slice; ManagedSession.snapshotChainIDs is responsible
// for the locking.
package claudejsonl

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
)

// ChainIDsFunc returns the Claude session ID chain for a session, in
// oldest → newest order (matching ManagedSession.prevSessionIDs + current
// session ID). Re-evaluated on every LoadBefore call so the result always
// reflects the latest chain state.
type ChainIDsFunc func() []string

// Source is the claude-code JSONL-backed history.Source.
type Source struct {
	claudeDir string       // ~/.claude (or override) — empty disables the source
	cwd       string       // session workspace, used for fast path in projDirName
	chainIDs  ChainIDsFunc // produces the current session-ID chain
}

// New constructs a Source. If claudeDir is empty or chainIDs is nil, the
// Source degrades to a zero-result implementation (equivalent to history.Noop)
// so misconfiguration never produces a nil-pointer panic at call time.
func New(claudeDir, cwd string, chainIDs ChainIDsFunc) *Source {
	return &Source{claudeDir: claudeDir, cwd: cwd, chainIDs: chainIDs}
}

// LoadBefore returns up to `limit` entries strictly older than beforeMS,
// in chronological order. Walks the session chain newest → oldest and
// stops as soon as the limit is met or ctx is cancelled.
func (s *Source) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error) {
	if limit <= 0 {
		return nil, nil
	}
	if s == nil || s.claudeDir == "" || s.chainIDs == nil {
		return nil, nil
	}
	ids := s.chainIDs()
	if len(ids) == 0 {
		return nil, nil
	}
	// discovery returns entries in chronological order within each JSONL
	// and flattens oldest-chain-first; the strict-< filter is applied
	// against each line during the reverse read, so we never have to
	// post-filter here.
	return discovery.LoadHistoryChainBeforeCtx(ctx, s.claudeDir, ids, s.cwd, beforeMS, limit), nil
}
