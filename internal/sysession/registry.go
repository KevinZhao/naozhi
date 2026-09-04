package sysession

import (
	"fmt"
)

// Built-in daemon names: single source of truth for builtinDaemons, each
// daemon's Name(), and cmd/naozhi config wiring (#1634).
const (
	DaemonAutoTitler   = "auto-titler"
	DaemonAttachmentGC = "attachment-gc"
)

// validateDaemonName enforces the kebab-case naming convention RFC §3.2:
//
//	^[a-z][a-z0-9-]{1,30}$
//
// Hand-written to avoid a regexp.MustCompile at package init.
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

// builtinDaemonFactory builds a Daemon from runtime dependencies (Router,
// Runner, DaemonConfig), none of which exist at package init. To register a
// new daemon:
//
//  1. Implement Daemon (and optionally Configurable).
//  2. Append a builtinDaemonFactory{Name: ..., Build: ...} to builtinDaemons.
//  3. Add a sane default to sysession.Config.Daemons.
type builtinDaemonFactory struct {
	Name  string
	Build func(deps DaemonDeps) (Daemon, error)
}

// DaemonDeps bundles runtime dependencies handed to each daemon's Build.
type DaemonDeps struct {
	Router SystemSessionRouter
	Runner Runner
	Cfg    DaemonConfig
	// WorkspaceRoots is non-nil only for daemons that sweep workspace
	// attachment dirs (attachment-gc). Other daemons ignore it.
	WorkspaceRoots WorkspaceRootLister
}

// builtinDaemons is the immutable list of compiled-in daemons; order pins
// startup order so tests are deterministic. It is deliberately a static slice
// literal, NOT an init()-driven registry like cli/history: every daemon is
// compiled into this package, so there is no import cycle to break and no
// external plugin surface. TestBuiltinDaemonsSliceLiteralInvariant pins this
// (#1055).
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

// validateBuiltinDaemonNames panics (from NewManager) if any compiled-in
// daemon name violates the kebab-case rule or duplicates another. A runtime
// check rather than a test so forks that add their own daemons hit it too.
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
