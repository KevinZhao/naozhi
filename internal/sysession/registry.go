package sysession

import (
	"fmt"
)

// Built-in daemon names. These are the single source of truth for the
// kebab-case identifiers used in builtinDaemons, each daemon's Name(),
// and config-translation wiring (cmd/naozhi). Referencing the constant
// instead of a string literal makes a rename a compile-time concern
// rather than a silent drift between registry and wiring (#1634).
const (
	DaemonAutoTitler   = "auto-titler"
	DaemonAttachmentGC = "attachment-gc"
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
// R244-ARCH-18 (#1055): this is a static slice literal, NOT cli/history's
// blank-import + init()-driven registry.  The divergence is deliberate, so
// record it here rather than have it re-flagged as accidental inconsistency
// (mirroring our standing precedent of promoting an implicit decision to a
// documented anchor).  The two reasons cli/history adopts the init() pattern
// are both absent for sysession:
//
//  1. No import cycle to break.  Every built-in daemon (auto-titler,
//     attachment-gc, ...) is compiled into this same package, so there is no
//     peer package that would have to import the registry — nothing to
//     decouple via blank import.
//  2. No out-of-package daemon contract.  Registering a daemon is an
//     in-package slice append, exactly the three steps documented on
//     builtinDaemonFactory above; there is no external plugin surface that
//     would benefit from self-registration in an init().
//
// The holistic "should every subsystem share one unified Registry[T]"
// question is tracked separately under R244-ARCH-4 (#1058, internal/wireup);
// sysession deliberately does not pre-commit and stays on this slice literal
// until that decision lands.  TestBuiltinDaemonsSliceLiteralInvariant pins
// this so any future move to an init()-based registry is a deliberate edit
// of both that test and this comment.
var builtinDaemons = []builtinDaemonFactory{
	{
		Name: DaemonAutoTitler,
		Build: func(deps DaemonDeps) (Daemon, error) {
			return newAutoTitler(deps)
		},
	},
	{
		Name: DaemonAttachmentGC,
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
