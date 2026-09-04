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
// <root>/.naozhi/attachments subtree attachment-gc sweeps: the router default
// workspace ∪ every per-chat workspace override ∪ every bound project Path
// (docs/rfc/attachment-gc-daemon.md §4.4 E1). It is intentionally NOT derived
// from the live session table — pruned sessions' attachment dirs are exactly
// the ones most in need of GC, and roots are stable across prune.
//
// Implementations MUST return paths already normalised+deduplicated (abs +
// EvalSymlinks) so the same directory is not double-swept via two spellings
// (E2). nil means "nothing to sweep this tick".
type WorkspaceRootLister interface {
	KnownWorkspaceRoots() []string
}

const (
	attachmentGCDefaultUploadTTL  = 7 * 24 * time.Hour
	attachmentGCDefaultRefTTL     = attachment.DefaultRefTTL // 30d
	attachmentGCDefaultPerRootCap = 500
	attachmentGCDefaultMetaGrace  = 5 * time.Minute
	// AttachmentGCMinTick floors the configured tick so a misconfigured short
	// interval cannot re-walk every attachment dir continuously. Wiring layers
	// should reference this constant instead of inlining time.Hour.
	AttachmentGCMinTick = time.Hour
)

// attachmentGC is the refcount-aware attachment reaper: a pure filesystem
// sweeper with no LLM Runner. See docs/rfc/attachment-gc-daemon.md.
type attachmentGC struct {
	roots WorkspaceRootLister

	uploadTTL  time.Duration
	refTTL     time.Duration
	perRootCap int
	metaGrace  time.Duration
	dryRun     bool

	// cursor is the round-robin start offset across roots so a high-churn root
	// that keeps hitting perRootCap cannot starve the others (E3). In-memory
	// only; GCWithRefs is idempotent so reset-on-restart is harmless.
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
	// A nil WorkspaceRoots degrades Tick to a logged no-op rather than failing
	// construction, so a misconfigured host still boots.
	return a, nil
}

func (a *attachmentGC) Name() string        { return DaemonAttachmentGC }
func (a *attachmentGC) Description() string { return "回收超过 TTL 且无引用的附件文件" }

// Configure reads the attachment-gc knobs; unknown keys are ignored.
// Validates ref_ttl >= upload_ttl.
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

// Tick sweeps every known workspace root once. Per-root budget + round-robin
// cursor bound per-tick deletion work and prevent root starvation (RFC §4.3);
// ctx is honoured between roots and, via GCWithRefs, inside a root's walk.
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
				// Cancelled mid-sweep: the root did not complete, so it is not counted as examined.
				firstErr = err
				break
			}
			metrics.AttachmentGCErrorTotal.Add(1)
			slog.Warn("attachment-gc: sweep failed", "root", root, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			// A real (non-cancel) error still counts as examined.
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
			// Cap hit; cursor already advanced so a starved root goes first next tick.
			report.Skipped["cap_hit"]++
		}
	}
	return report, firstErr
}

// recordWouldReap fans per-reason would-remove counts out to the bucketed
// expvar counters (RFC §6 E4), in both dry-run and live mode.
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
