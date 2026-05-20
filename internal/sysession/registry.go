package sysession

import (
	"fmt"
	"regexp"
)

// daemonNameRE locks the kebab-case naming convention RFC §3.2:
//
//	^[a-z][a-z0-9-]{1,30}$
//
// Lower-case ASCII only, leading letter, length 2..32.  No dots so we
// can grow nested namespaces (sys:foo.bar) later without retroactively
// allowing them today.  No leading digit so a future numeric daemon
// version (auto-titler-2) doesn't collide with a "2-something" name.
var daemonNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

// validateDaemonName returns an error when name violates the kebab-case
// naming rule.  Called from NewManager at startup; a single bad name
// halts process start (panic) rather than producing a half-functional
// Manager — this is exactly the kind of misconfiguration that should be
// a build-blocker, not a runtime degradation.
func validateDaemonName(name string) error {
	if !daemonNameRE.MatchString(name) {
		return fmt.Errorf("sysession: daemon name %q must match %s",
			name, daemonNameRE.String())
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
}

// builtinDaemons is the immutable list of compiled-in daemons.  Order
// determines startup order (which doesn't matter for Phase 1 since
// daemons are independent, but pinning it lets tests assert
// deterministic behaviour).
//
// Phase 1 is shipped with AutoTitler only.  TransientSweeper / other
// future daemons land in Phase 2 (RFC §12).
var builtinDaemons = []builtinDaemonFactory{
	{
		Name: "auto-titler",
		Build: func(deps DaemonDeps) (Daemon, error) {
			return newAutoTitler(deps)
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
