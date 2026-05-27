// Package keyspec is the zero-dependency single source of truth for
// session-key namespace literals shared between internal/session and
// internal/project. R239-ARCH-G (#900): "project:{name}:planner" was
// previously hardcoded in BOTH internal/session/key.go (as
// plannerKeyFor) and internal/project (as PlannerKeyFor). Cross-module
// drift was caught only by a hand-written cross-package literal test —
// the kind of "keep in sync" scaffolding RFC drift reviews repeatedly
// flagged for elimination.
//
// This package owns the prefix + suffix constants and the canonical
// constructors. internal/session migrates to the keyspec helpers
// directly; internal/project continues to expose its existing
// public API and can switch its implementation to delegate here in
// a follow-up commit (the literals match by construction so the
// existing format-locked test in project_test.go continues to pass
// during the transition).
//
// Zero-dep: this package imports nothing from internal/* so any
// internal/session or internal/project consumer can take it without
// creating an import cycle.
package keyspec

import "strings"

// ProjectKeyPrefix is the namespace prefix for project-scoped session
// keys. The canonical key shape is `project:{name}:planner`. Mirrors
// the constant of the same name in internal/session — both packages
// reference this single source of truth.
const ProjectKeyPrefix = "project:"

// PlannerKeySuffix is the trailing token that distinguishes a planner
// key from any future `project:{name}:<role>` sub-roles. Today every
// `project:` key is a planner key, but the constant exists so adding
// a new role (e.g. `project:foo:tasks`) does not require rewriting
// the suffix-match logic in two places.
const PlannerKeySuffix = ":planner"

// PlannerKeyFor returns the canonical planner session key for the
// given project name. Callers must have validated `name` against the
// project name regex (see internal/project.ValidateProjectName) —
// keyspec performs no validation so it can stay zero-dep.
//
// Format: `project:{name}:planner`. Migration of existing literals
// must continue to satisfy:
//
//	PlannerKeyFor("foo") == "project:foo:planner"
//
// which is asserted by both internal/project and internal/session
// format-locked tests.
func PlannerKeyFor(name string) string {
	return ProjectKeyPrefix + name + PlannerKeySuffix
}

// IsPlannerKey reports whether the given key is a planner session
// key. Returns false for both the empty-name edge case
// (`project::planner`) and any key missing the prefix or suffix.
//
// The empty-name rejection mirrors the pre-extraction logic so that
// any caller migrating to keyspec sees identical behaviour.
func IsPlannerKey(key string) bool {
	if !strings.HasPrefix(key, ProjectKeyPrefix) {
		return false
	}
	if !strings.HasSuffix(key, PlannerKeySuffix) {
		return false
	}
	// Reject the boundary case "project::planner" — the {name} segment
	// must be non-empty for the key to identify a real project.
	return len(key) > len(ProjectKeyPrefix)+len(PlannerKeySuffix)
}

// PlannerNameFromKey extracts {name} from a planner key. Returns the
// empty string for any non-planner input (including keys shorter than
// prefix+suffix, missing prefix/suffix, or the empty-name edge case
// `project::planner`).
//
// R20260526-CR-009: previously the function sliced unconditionally and
// would panic on `slice bounds out of range` when given a too-short
// input. The godoc told callers to gate on IsPlannerKey first, but a
// silent caller bug would crash with an uninformative runtime panic.
// The self-defense IsPlannerKey gate makes the function total: well-
// formed callers pay one extra HasPrefix+HasSuffix check (cheap), ill-
// formed callers get a typed empty-string instead of a panic.
func PlannerNameFromKey(key string) string {
	if !IsPlannerKey(key) {
		return ""
	}
	return key[len(ProjectKeyPrefix) : len(key)-len(PlannerKeySuffix)]
}
