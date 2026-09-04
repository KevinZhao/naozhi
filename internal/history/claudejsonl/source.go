// Package claudejsonl implements history.Source on top of the Claude Code
// CLI's per-session JSONL transcripts under ~/.claude/projects/.
//
// A session key may span several Claude session IDs (a "chain"; the CLI
// rotates IDs on /new, --resume, workspace switches). LoadBefore walks the
// chain newest → oldest via discovery.LoadHistoryChainBeforeCtx. The chain is
// supplied through a callback so a page in flight sees the latest chain.
package claudejsonl

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/discovery"
)

// ChainIDsFunc returns the Claude session ID chain, oldest → newest.
// Re-evaluated on every LoadBefore call.
type ChainIDsFunc func() []string

// Source is the claude-code JSONL-backed history.Source.
type Source struct {
	claudeDir string // ~/.claude (or override) — empty disables the source
	cwd       string
	chainIDs  ChainIDsFunc
}

// New constructs a Source. Empty claudeDir or nil chainIDs yields a
// zero-result Source rather than a nil-pointer panic.
func New(claudeDir, cwd string, chainIDs ChainIDsFunc) *Source {
	return &Source{claudeDir: claudeDir, cwd: cwd, chainIDs: chainIDs}
}

// init registers the claude history factory with cli and wires
// discovery.ThumbnailFn (the sole injection point). Without ThumbnailFn image
// blocks in rehydrated JSONL history are silently dropped — no build error.
func init() {
	cli.RegisterHistoryFactory("claude", factory)
	discovery.ThumbnailFn = cli.MakeThumbnail
}

// factory returns cli.NoopHistorySource when the wiring lacks a ClaudeDir so
// a router-level misconfig still yields a non-nil source.
func factory(s cli.HistorySessionView, deps cli.HistoryWiring) cli.HistorySource {
	if deps.ClaudeDir == "" {
		return cli.NoopHistorySource{}
	}
	return New(deps.ClaudeDir, s.Workspace(), s.SnapshotChainIDs)
}

// LoadBefore returns up to `limit` entries strictly older than beforeMS, in
// chronological order, walking the session chain newest → oldest.
func (s *Source) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
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
	// discovery applies the strict-< filter during the reverse read and
	// flattens oldest-chain-first; no post-filter needed here.
	return discovery.LoadHistoryChainBeforeCtx(ctx, s.claudeDir, ids, s.cwd, beforeMS, limit), nil
}
