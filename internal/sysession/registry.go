package sysession

import (
	"fmt"
)

// validateDaemonName enforces the kebab-case naming convention RFC §3.2:
//
//	^[a-z][a-z0-9-]{1,30}$
//
// Lower-case ASCII only, leading letter, total length 2..31 (1 leading
// + 1..30 trailing chars). R236-PERF-3: hand-written check avoids a
// regexp.MustCompile at package init for the cold-path NewManager call.
func validateDaemonName(name string) error {
	if len(name) < 2 || len(name) > 31 {
		return fmt.Errorf("sysession: daemon name %q must be 2..31 chars (kebab-case)", name)
	}
	if c := name[0]; c < 'a' || c > 'z' {
		return fmt.Errorf("sysession: daemon name %q must start with a lowercase letter", name)
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return fmt.Errorf("sysession: daemon name %q must contain only lowercase letters, digits, and hyphens, start with a letter, total length 2..31", name)
		}
	}
	return nil
}

// builtinDaemonFactory builds a Daemon from runtime dependencies.  We
// return a factory rather than a value because daemons need access to
// Router, Runner, and per-daemon DaemonConfig — none of which exist at
// package init time.
//
// Each entry in the slice below corresponds to one compiled-in daemon.
// To register a new one:
//
//  1. Implement Daemon (and optionally Configurable).
//  2. Append a builtinDaemonFactory{Name: ..., Build: ...} below.
//  3. Add a sane default to sysession.Config.Daemons so operators can
//     opt in without re-reading the source.
type builtinDaemonFactory struct {
	Name  string
	Build func(deps DaemonDeps) (Daemon, error)
}

// DaemonDeps bundles runtime dependencies handed to each daemon's
// Build function.  Keeps the factory signature stable when we grow
// dependencies later.
type DaemonDeps struct {
	Router SystemSessionRouter
	Runner Runner
	Cfg    DaemonConfig
	// WorkspaceRoots is non-nil only for daemons that sweep workspace
	// attachment dirs (attachment-gc). Other daemons ignore it.
	WorkspaceRoots WorkspaceRootLister
}

// builtinDaemons is the immutable list of compiled-in daemons.  Order
// determines startup order (which doesn't matter for Phase 1 since
// daemons are independent, but pinning it lets tests assert
// deterministic behaviour).
//
// Phase 1 is shipped with AutoTitler only.  TransientSweeper / other
// future daemons land in Phase 2 (RFC §12).
//
// R244-ARCH-18 (#1055) — why a slice literal here, NOT cli/history's
// blank-import + init()-registry pattern:
//
// cli/history uses RegisterHistoryFactory from each backend package's
// init() for two reasons that DO NOT apply to sysession (see
// internal/cli/history.go godoc):
//
//  1. Cycle avoidance. internal/history/claudejsonl imports internal/cli
//     for cli.EventEntry; if cli registered backends by importing them
//     the build would cycle. sysession's daemons (auto-titler,
//     attachment-gc) are ALL compiled into this same package — there is
//     no peer package to import, hence no cycle to break.
//  2. Out-of-package extensibility. history backends each live in their
//     own package so a new one lands as a single new file with an
//     init() block. sysession has no out-of-package daemon contract;
//     adding one is an in-package slice append (see the registration
//     steps on builtinDaemonFactory).
//
// A blank-import registry would buy nothing here (no cycle, no external
// backends) while costing the static, grep-able, test-swappable
// (withRegistry) slice the Manager tests rely on. The holistic
// "unified Registry across cron daemons / platforms / backends / wireup"
// question is tracked separately under R244-ARCH-4 (#1058, still open);
// sysession deliberately stays on the slice literal until that design
// lands rather than pre-committing one side of it. This anchor exists so
// the style divergence stops being re-flagged as an accidental
// inconsistency — it is a deliberate, locally-correct choice (mirrors the
// StopPolicyForceExit / #1060 "promote the implicit decision to a
// documented anchor" precedent in manager.go).
var builtinDaemons = []builtinDaemonFactory{
	{
		Name: "auto-titler",
		Build: func(deps DaemonDeps) (Daemon, error) {
			return newAutoTitler(deps)
		},
	},
	{
		Name: "attachment-gc",
		Build: func(deps DaemonDeps) (Daemon, error) {
			return newAttachmentGC(deps)
		},
	},
}

// validateBuiltinDaemonNames panics if any compiled-in daemon name
// violates the kebab-case rule or duplicates another.  Called from
// NewManager so a misconfiguration becomes a startup failure rather
// than a quiet runtime degradation.
//
// We could do this in a TestMain init, but a runtime check covers
// downstream consumers that compile a fork with their own daemons
// added — they'd skip our test suite but still hit this panic.
func validateBuiltinDaemonNames() {
	seen := make(map[string]struct{}, len(builtinDaemons))
	for _, f := range builtinDaemons {
		if err := validateDaemonName(f.Name); err != nil {
			panic(err)
		}
		if _, dup := seen[f.Name]; dup {
			panic(fmt.Sprintf("sysession: duplicate built-in daemon name %q", f.Name))
		}
		seen[f.Name] = struct{}{}
	}
}
