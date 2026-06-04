// Package session_test — projectapi_alias_test.go
//
// Compile-time guard that session.ProjectBinding / session.PlannerDataSource
// remain TYPE ALIASES for the canonical projectapi definitions, not
// independent redeclarations. R260528-ARCH-12 (#1373).
//
// Why this matters: the decoupling that lets internal/project drop its
// reverse-import of internal/session relies on the two type names being the
// SAME type. If a future edit replaced the alias (`type ProjectBinding =
// projectapi.ProjectBinding`) with a distinct struct (`type ProjectBinding
// struct {...}`), the session package would still compile, dispatch's
// `session.PlannerDataSource` wiring would still compile, but
// project.NewDataSource (which now returns projectapi.DataSource) would no
// longer satisfy session.PlannerDataSource — breaking the wiring with a
// confusing cross-package error far from the cause. These assignments force
// any such drift to fail HERE with a pointed compile error.
package session_test

import (
	"github.com/naozhi/naozhi/internal/projectapi"
	"github.com/naozhi/naozhi/internal/session"
)

// Alias identity: assigning in both directions only compiles when the two
// names denote the same type (a defined-type redeclaration would require an
// explicit conversion and fail these bare assignments).
var (
	_ session.ProjectBinding    = projectapi.ProjectBinding{}
	_ projectapi.ProjectBinding = session.ProjectBinding{}

	// PlannerDataSource alias identity: a projectapi.DataSource value must be
	// usable wherever session.PlannerDataSource is expected and vice-versa.
	_ = func(d projectapi.DataSource) session.PlannerDataSource { return d }
	_ = func(d session.PlannerDataSource) projectapi.DataSource { return d }
)
