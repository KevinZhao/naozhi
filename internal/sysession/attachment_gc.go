package sysession

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/naozhi/naozhi/internal/attachment"
	"github.com/naozhi/naozhi/internal/metrics"
)

// WorkspaceRootLister enumerates the distinct workspace roots whose
// <root>/.naozhi/attachments subtree the attachment-gc daemon sweeps.
//
// The set is the UNION of (docs/rfc/attachment-gc-daemon.md §4.4 E1):
//   - the router default workspace,
//   - every per-chat workspace override value,
//   - every bound project's Path.
//
// It is intentionally NOT derived from the live session table: that set
// shrinks as sessions are pruned, but the attachment dirs of pruned
// sessions are exactly the ones most in need of GC. Workspace *roots*
// are stable across prune, so enumerating roots covers dead-session
// attachments too.
//
// Implementations MUST return paths already normalised+deduplicated
// (abs + EvalSymlinks) so the daemon does not double-sweep the same
// directory reached via two different strings (E2). Returning nil is
// allowed and means "nothing to sweep this tick".
type WorkspaceRootLister interface {
	KnownWorkspaceRoots() []string
}

const (
	attachmentGCDefaultUploadTTL  = 7 * 24 * time.Hour
	attachmentGCDefaultRefTTL     = attachment.DefaultRefTTL // 30d
	attachmentGCDefaultPerRootCap = 500
	attachmentGCDefaultMetaGrace  = 5 * time.Minute
	// AttachmentGCMinTick floors the configured tick so a misconfigured
	// short interval (e.g. 30s) cannot make the daemon re-walk every
	// attachment dir continuously. GC is low-frequency by nature.
	// Wiring layers (e.g. cmd/naozhi/main_helpers.go) should reference
	// this constant instead of inlining time.Hour.
	AttachmentGCMinTick = time.Hour
)

// attachmentGC is the refcount-aware attachment reaper daemon. It owns
// no LLM Runner — it is a pure filesystem sweeper, the first member of
// the sysession §12 "sweeper" family. See docs/rfc/attachment-gc-daemon.md.
type attachmentGC struct {
	roots WorkspaceRootLister

	uploadTTL  time.Duration
	refTTL     time.Duration
	perRootCap int
	metaGrace  time.Duration
	dryRun     bool

	// cursor is the round-robin start offset across roots so a single
	// high-churn root that repeatedly hits perRootCap cannot starve the
	// others (E3). In-memory only; reset to 0 on restart is harmless
	// because GCWithRefs is idempotent.
	cursor int

	// nowFn is injected in tests; nil → time.Now.
	nowFn func() time.Time
}

func newAttachmentGC(deps DaemonDeps) (Daemon, error) {
	a := &attachmentGC{
		roots:      deps.WorkspaceRoots,
		uploadTTL:  attachmentGCDefaultUploadTTL,
		refTTL:     attachmentGCDefaultRefTTL,
		perRootCap: attachmentGCDefaultPerRootCap,
		metaGrace:  attachmentGCDefaultMetaGrace,
	}
	// deps.WorkspaceRoots may be nil if the host did not wire it; Tick
	// degrades to a logged no-op rather than failing construction, so a
	// misconfigured host still boots.
	return a, nil
}

func (a *attachmentGC) Name() string        { return "attachment-gc" }
func (a *attachmentGC) Description() string { return "回收超过 TTL 且无引用的附件文件" }

// Configure reads the attachment-gc knobs. Unknown keys ignored
// (forward-compat). Validates ref_ttl >= upload_ttl. Clamps tick-adjacent
// numeric knobs defensively.
func (a *attachmentGC) Configure(cfg DaemonConfig) error {
	if v, ok := cfg["upload_ttl"].(time.Duration); ok && v > 0 {
		a.uploadTTL = v
	}
	if v, ok := cfg["ref_ttl"].(time.Duration); ok && v > 0 {
		a.refTTL = v
	}
	if v, ok := cfg["per_root_cap"].(int); ok && v > 0 {
		a.perRootCap = v
	}
	if v, ok := cfg["meta_grace"].(time.Duration); ok {
		a.metaGrace = v
	}
	if v, ok := cfg["dry_run"].(bool); ok {
		a.dryRun = v
	}
	if a.refTTL < a.uploadTTL {
		return fmt.Errorf("attachment-gc: ref_ttl(%s) < upload_ttl(%s) 无意义", a.refTTL, a.uploadTTL)
	}
	return nil
}

func (a *attachmentGC) now() time.Time {
	if a.nowFn != nil {
		return a.nowFn()
	}
	return time.Now()
}

// Tick sweeps every known workspace root once. Per-root budget +
// round-robin cursor bound the per-tick deletion work and prevent
// root starvation (RFC §4.3 / §4.6-2). ctx is honoured both between
// roots and (via GCWithRefs) inside a single root's walk.
func (a *attachmentGC) Tick(ctx context.Context) (TickReport, error) {
	metrics.AttachmentGCSweepTotal.Add(1)

	if a.roots == nil {
		slog.Warn("attachment-gc: no WorkspaceRootLister wired; skipping")
		return TickReport{}, nil
	}
	roots := a.roots.KnownWorkspaceRoots()
	if len(roots) == 0 {
		return TickReport{}, nil
	}

	now := a.now()
	report := TickReport{Skipped: map[string]int{}}
	start := a.cursor % len(roots)

	var firstErr error
	for i := 0; i < len(roots); i++ {
		if err := ctx.Err(); err != nil {
			firstErr = err
			break
		}
		root := roots[(start+i)%len(roots)]
		a.cursor++ // advance regardless so next tick starts elsewhere
		if root == "" || !filepath.IsAbs(root) {
			report.Skipped["bad_root"]++
			continue
		}

		res, err := attachment.GCWithRefs(ctx, root, attachment.GCOptions{
			UploadTTL: a.uploadTTL,
			RefTTL:    a.refTTL,
			Now:       now,
			MaxRemove: a.perRootCap,
			MetaGrace: a.metaGrace,
			DryRun:    a.dryRun,
		})
		if err != nil {
			if ctx.Err() != nil {
				// Context cancelled mid-sweep: do NOT count the root as
				// examined — the sweep did not complete. [R20260601-CR-6]
				firstErr = err
				break
			}
			metrics.AttachmentGCErrorTotal.Add(1)
			slog.Warn("attachment-gc: sweep failed", "root", root, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			// Still count as examined: we attempted and got a real error
			// (not a cancel), so the root was processed this tick.
			report.Examined++
			recordWouldReap(res)
			continue
		}
		report.Examined++
		recordWouldReap(res)
		if !a.dryRun {
			metrics.AttachmentGCReapedTotal.Add(int64(res.Removed))
			report.Acted += res.Removed
		}
		if res.Stopped {
			// Per-root cap hit; cursor already advanced so a starved
			// root gets first crack next tick.
			report.Skipped["cap_hit"]++
		}
	}
	return report, firstErr
}

// recordWouldReap fans the per-reason would-remove counts out to the
// bucketed expvar counters (RFC §6 E4). Called in both dry-run and live
// mode for observability.
func recordWouldReap(res attachment.GCResult) {
	for reason, n := range res.WouldRemove {
		switch reason {
		case attachment.ReasonLegacyNoMeta:
			metrics.AttachmentGCWouldReapLegacyTotal.Add(int64(n))
		case attachment.ReasonMetaNoRefs:
			metrics.AttachmentGCWouldReapNoRefsTotal.Add(int64(n))
		case attachment.ReasonRefsExpired:
			metrics.AttachmentGCWouldReapExpiredTotal.Add(int64(n))
		}
	}
}
