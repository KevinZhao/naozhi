// Package datadir owns the on-disk layout policy for naozhi's state: given the
// directory a store file lives in, it is the only place that knows what the
// siblings are called. R250-ARCH-13 (#1175), F1 (#2641).
//
// # Why the first version was never adopted
//
// This package shipped as six free functions of the shape `f(dataDir) string`,
// four of which had zero callers — production AND test — while ~18 sites
// open-coded `filepath.Dir(storePath) + "/xxx"` instead. The reason was not
// neglect: the API asked a question the configuration cannot answer.
//
// There is no data root in the config. There are TWO independently configurable
// store files with no same-directory constraint: `session.store_path` and
// `cron.store_path`. So three of those functions were not merely unused, they
// were unusable:
//
//   - SessionsPath(root) rebuilt `<root>/sessions.json`, discarding the operator's
//     configured filename. Adopting it would have pointed the loader at a file
//     that does not exist.
//   - CronJobsPath(root) did the same for cron.
//   - CronRunsRoot(root) assumed cron state sits under the SESSION root, while
//     cron derives it from its own store path. Adopting it would have relocated
//     run history for anyone who split the two.
//
// EnsureDir, UISettingsPath and CLIDebugRoot were adopted precisely because
// they do not invert that relationship.
//
// # The shape that works
//
// Layout wraps ONE store directory, constructed either from a store file path
// (ForStore) or from a root that the caller already holds (FromRoot). A single
// Layout carrying both store paths was rejected: it would put RunsRoot() one
// method call away from reading the session root, which is the exact miswiring
// above. One Layout per store file makes that unrepresentable.
//
// A configured path is never rebuilt from a root — Layout only ever derives
// SIBLINGS. The store file path itself stays whatever the operator wrote.
package datadir

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// DirMode is the contractual mode for every naozhi-owned data directory.
// 0o700 keeps session state, event logs, and cron job/run JSON (which embed
// script source, env values, and output summaries) unreadable by other OS
// users on a shared host.
const DirMode fs.FileMode = 0o700

// Layout is the directory one store file lives in, plus the names of its
// siblings. Value type with one field: there is no nil Layout, no constructor
// that can fail, and nothing to nil-guard at an injection site (the typed-nil
// hazard of #377 / #2551 / #2561 cannot arise).
//
// The zero Layout has an empty root and every method returns "" — the same
// degrade-quietly behaviour the open-coded sites had when the store path was
// unset, and what EnsureDir already treats as a no-op.
type Layout struct {
	root string
}

// ForStore returns the Layout of the directory holding storePath. This is the
// constructor for the two configured store files; the file's own name is the
// caller's to keep.
func ForStore(storePath string) Layout {
	if storePath == "" {
		return Layout{}
	}
	return Layout{root: filepath.Dir(storePath)}
}

// FromRoot returns the Layout of an already-known state root, for callers that
// are handed the directory rather than a store file (ServerOptions.StateDir).
func FromRoot(root string) Layout {
	return Layout{root: root}
}

// Root is the directory itself.
func (l Layout) Root() string { return l.root }

// Join names a path inside the layout. Empty root yields "" rather than a
// root-relative path, so an unset store path cannot turn into a write at the
// filesystem root.
func (l Layout) Join(parts ...string) string {
	if l.root == "" {
		return ""
	}
	return filepath.Join(append([]string{l.root}, parts...)...)
}

// Session-store siblings.

// SessionIDsPath is the known-session-ID ledger (<root>/session-ids.json).
func (l Layout) SessionIDsPath() string { return l.Join("session-ids.json") }

// WorkspaceOverridesPath is the per-session workspace override file.
func (l Layout) WorkspaceOverridesPath() string { return l.Join("workspace-overrides.json") }

// EventsRoot is the per-session event-log directory (<root>/events).
func (l Layout) EventsRoot() string { return l.Join("events") }

// CostRoot is the cost-ledger directory (<root>/cost).
func (l Layout) CostRoot() string { return l.Join("cost") }

// SessionRunsRoot is the per-session run-record directory
// (<root>/session-runs). Sole definition of that name: runhistory.NewStore
// takes this path rather than appending the segment itself, so the cost
// reporter and the store cannot disagree about where records live.
func (l Layout) SessionRunsRoot() string { return l.Join("session-runs") }

// CLIDebugRoot is the per-session CLI debug-log directory (<root>/cli-debug).
// Populated only when the operator opts in via NAOZHI_CLI_DEBUG; it holds raw
// `claude --debug-file` output (HTTP request/response + retry status codes),
// so a leaked file would expose prompt/tool internals — hence the same 0o700
// EnsureDir hardening as the other state roots.
func (l Layout) CLIDebugRoot() string { return l.Join("cli-debug") }

// AccessProfileSecretsRoot holds per-profile secret material.
func (l Layout) AccessProfileSecretsRoot() string { return l.Join("access-profile-secrets") }

// SysSessionsRoot is the sysession runner's working directory
// (<root>/sys-sessions). Every sysession consumer MUST agree on it: it is the
// history panel's SkipWorkspace filter target, so a JSONL landing anywhere else
// leaks into the history list.
func (l Layout) SysSessionsRoot() string { return l.Join("sys-sessions") }

// NaozhiSettingsPath is the generated Claude settings file naozhi passes via
// --settings (<root>/naozhi-settings.json).
func (l Layout) NaozhiSettingsPath() string { return l.Join("naozhi-settings.json") }

// UISettingsPath is the dashboard UI-preferences file (<root>/ui-settings.json).
// Operator-chosen presentation state (today: theme) that used to live only in
// browser localStorage; persisting it server-side lets the choice survive a
// browser/device change or a cache clear. Single-user model: one file per
// instance (docs note in internal/uiprefs).
func (l Layout) UISettingsPath() string { return l.Join("ui-settings.json") }

// Cron-store siblings.

// RunsRoot is the cron run-record root (<root>/runs), with per-job
// subdirectories beneath it. Derived from the CRON store's layout — callers
// must build this Layout from cron.store_path, never from the session's.
func (l Layout) RunsRoot() string { return l.Join("runs") }

// EnsureDir creates path (and parents) at DirMode and tightens it down to a
// safe state, returning an error only when path cannot be made usable as a
// private directory.
//
// Steps:
//  1. MkdirAll(path, 0o700) — fails hard if the directory can't be created.
//  2. Lstat the leaf: reject a symlink or non-directory (a planted
//     <dataDir>/X → /etc symlink would otherwise silently redirect every
//     subsequent write outside the data root; MkdirAll does not error on a
//     symlink-to-dir). This is the authoritative redirect guard.
//  3. Chmod the leaf to 0o700 when it carries looser perms. MkdirAll only
//     applies perm to directories it actually creates, so a pre-existing
//     0o755 tree keeps its mode without this step. Chmod failure is logged
//     and tolerated (containers with read-only / non-owned bind mounts can't
//     chmod) — the Lstat redirect guard, not the mode, is the security
//     boundary.
//
// Empty path is a no-op (nil) so callers that derive the path from an
// unset data root degrade quietly, matching the prior os.MkdirAll-guarded
// call sites.
func EnsureDir(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(path, DirMode); err != nil {
		return fmt.Errorf("create data directory %q: %w", path, err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat data directory %q: %w", path, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("data directory %q is a symlink or non-directory (mode %s); refusing to use", path, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != DirMode {
		if cerr := os.Chmod(path, DirMode); cerr != nil {
			slog.Warn("datadir: chmod to 0700 failed; leaving prior mode",
				"path", path, "had_mode", perm.String(), "err", cerr)
		} else {
			slog.Info("datadir: corrected directory mode to 0700",
				"path", path, "had_mode", perm.String())
		}
	}
	return nil
}
