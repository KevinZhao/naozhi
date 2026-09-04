// Package timeouts is the canonical home for naozhi timeout / deadline
// constants.
//
// Package-scope `var defaultFooTimeout = 30 * time.Second` declarations
// scattered across packages are var-as-const anti-patterns: production code
// can scribble them at runtime, each test override grows its own
// save/restore boilerplate, and operators must grep many packages to find
// the governing timeout. [Get] returns a fresh copy of [Defaults] as the
// single source of truth; tests flip one field with [Override], which
// restores it via t.Cleanup under the package mutex (#662).
//
// New code MUST add a field here instead of declaring its own package-scope
// `var`; existing sites migrate one PR at a time.
package timeouts
