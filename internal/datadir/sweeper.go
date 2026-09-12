package datadir

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// sweeper.go — retention for the data-dir trees nothing else prunes. J6 of #2548.
//
// naozhi has eight retention mechanisms and they are not the same kind of thing.
// Five of them trim a live structure as a side effect of writing to it:
// runhistory trims its ring on warm, cron's runstore trims on append, eventlog
// rotates on write, the attachment tracker coalesces on its own tick, the
// retired store prunes on the server's ticker. Those belong where they are —
// they need the writer's lock and its notion of "current".
//
// What was missing is retention for directories that only ever accumulate,
// where no writer is in a position to clean up after itself because the files
// outlive the process that made them:
//
//	cli-debug/   one <keyhash>.log per session key, appended for the session's life
//	shims/       one shim-<pid>.log per shim process, orphaned when it exits
//	sys-sessions/ one JSONL per transient system session (~2880/day at a 30s tick)
//
// Measured on a live instance before this existed: cli-debug/ at 78 MB across
// 265 files with the oldest six weeks old, and shims/ at 909 files with the
// oldest five months old. The shim case is not merely disk: StartShim returns
// ErrStateDirQuotaExceeded when the state dir is over quota, so a big enough
// backlog of dead shims' logs makes new sessions fail to start (#456).
//
// sys-sessions had a sweep already, but it ran exactly once per process at
// startup (cmd/naozhi/main_helpers.go), so an instance up for weeks never swept.
//
// A Pass is deliberately shallow — top-level regular files of one directory,
// filtered by extension — so a sweep can only ever remove files naozhi itself
// named. Deciding whether a file is still in use is the registering package's
// job, not this one's: see Pass.Keep.

// Pass is one gardening pass over one directory. The zero value sweeps nothing.
type Pass struct {
	// Name identifies the pass in logs, e.g. "cli-debug".
	Name string
	// Dir is swept non-recursively. A missing directory is not an error: the
	// tree may not have been created yet.
	Dir string
	// MaxAge bounds a file's mtime. Zero or negative disables the pass entirely,
	// matching the existing "0 disables" convention for sysession's jsonl_max_age.
	MaxAge time.Duration
	// Ext restricts the sweep to one extension, including the dot (".log"). It
	// is required: sweeping every file in a directory would remove files a future
	// naozhi or CLI drops there on behalf of behaviour this package does not
	// control.
	Ext string
	// Keep, when non-nil, is consulted for every candidate that is already past
	// MaxAge; returning true keeps the file. This is where "still in use" lives,
	// because only the owning package can answer it — shim, for instance, checks
	// whether the pid in shim-<pid>.log is still alive.
	Keep func(name string) bool
}

// Result reports what one Pass removed.
type Result struct {
	Removed int
	Bytes   int64
}

// Run performs one pass. The error is non-nil only when the directory itself
// cannot be read; per-file failures are logged and skipped, because one
// undeletable file must not stop the rest of the sweep.
func (p Pass) Run() (Result, error) {
	var res Result
	if p.Dir == "" || p.MaxAge <= 0 || p.Ext == "" {
		return res, nil
	}
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, fmt.Errorf("datadir: read sweep dir %q: %w", p.Dir, err)
	}
	cutoff := time.Now().Add(-p.MaxAge)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != p.Ext {
			continue
		}
		info, err := e.Info()
		if err != nil {
			slog.Warn("sweep: stat entry failed", "pass", p.Name, "dir", p.Dir, "entry", name, "err", err)
			continue
		}
		// Regular files only. This is not about symlinks — os.Remove on a link
		// removes the link, never the target — it is about fifos, sockets and
		// device nodes, which os.Remove WOULD delete. A sweep owns the regular
		// files naozhi named here and nothing else. (ReadDir's Info is Lstat-based,
		// so a symlink is also excluded, and its target is untouched either way.)
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if p.Keep != nil && p.Keep(name) {
			continue
		}
		size := info.Size()
		if err := os.Remove(filepath.Join(p.Dir, name)); err != nil {
			slog.Warn("sweep: remove entry failed", "pass", p.Name, "dir", p.Dir, "entry", name, "err", err)
			continue
		}
		res.Removed++
		res.Bytes += size
	}
	return res, nil
}

// Sweeper runs registered passes on one shared ticker, so adding a tree to
// garden does not add a goroutine.
type Sweeper struct {
	interval time.Duration

	mu     sync.Mutex
	passes []Pass
	last   map[string]Result // cumulative per pass, for reporting
}

// NewSweeper returns a Sweeper that runs every pass once per interval. A
// non-positive interval means Run does a single pass and returns, which is what
// one-shot callers (and tests) want.
func NewSweeper(interval time.Duration) *Sweeper {
	return &Sweeper{interval: interval, last: map[string]Result{}}
}

// Add registers a pass. Passes with a non-positive MaxAge are accepted and
// simply do nothing, so a caller can pass a config value through unguarded.
func (s *Sweeper) Add(p Pass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.passes = append(s.passes, p)
}

// RunOnce runs every registered pass and returns what each removed this time.
func (s *Sweeper) RunOnce() map[string]Result {
	s.mu.Lock()
	passes := make([]Pass, len(s.passes))
	copy(passes, s.passes)
	s.mu.Unlock()

	out := make(map[string]Result, len(passes))
	for _, p := range passes {
		res, err := p.Run()
		if err != nil {
			slog.Warn("sweep pass failed", "pass", p.Name, "err", err)
		}
		if res.Removed > 0 {
			slog.Info("sweep reclaimed", "pass", p.Name, "dir", p.Dir,
				"removed", res.Removed, "bytes", res.Bytes, "max_age", p.MaxAge)
		}
		out[p.Name] = res
	}

	s.mu.Lock()
	for name, res := range out {
		agg := s.last[name]
		agg.Removed += res.Removed
		agg.Bytes += res.Bytes
		s.last[name] = agg
	}
	s.mu.Unlock()
	return out
}

// Totals returns what each pass has reclaimed over this process's lifetime.
func (s *Sweeper) Totals() map[string]Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Result, len(s.last))
	for k, v := range s.last {
		out[k] = v
	}
	return out
}

// Run sweeps immediately, then once per interval until ctx is done. The
// immediate pass preserves the startup-sweep behaviour the sys-sessions tree had
// before it was registered here.
func (s *Sweeper) Run(ctx context.Context) {
	s.RunOnce()
	if s.interval <= 0 {
		return
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunOnce()
		}
	}
}
