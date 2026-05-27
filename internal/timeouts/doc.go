// Package timeouts is the canonical home for naozhi timeout / deadline
// constants.
//
// # Why this package exists (R247-ARCH-18, #662)
//
// A code-review across 14 packages found 80+ "looks-like-const-actually-var"
// declarations of the form
//
//	var defaultFooTimeout = 30 * time.Second
//
// at package scope. Each one was almost-always-immutable, but technically a
// `var` because one or two test files needed to override it. This is a
// well-known anti-pattern:
//
//   - The compiler cannot inline the value, costing a load per use.
//   - Production code can scribble the value at runtime by accident
//     (no `const` enforcement); a typo in a long-lived daemon would
//     silently leak a stale duration.
//   - Each test override grows a copy of the same "save / restore /
//     t.Cleanup" boilerplate which routinely forgets the restore on a
//     parallel test path.
//   - Operators who want to know "what timeout governs this code path"
//     have to grep 14 packages instead of one.
//
// # The minimal-viable shape (this commit)
//
// We expose [Defaults] as the single source of truth for the timeouts that
// already cluster around shared semantics. Each call returns a fresh value
// (struct copy) so tests can mutate the returned struct without leaking
// state between t.Parallel goroutines.
//
// Callers that need to override a single field for a test should use
// [Override], which records the previous value and registers a t.Cleanup
// to restore it. Override is goroutine-safe under the package mutex.
//
// # Migration policy
//
// New code MUST use timeouts.Defaults() instead of declaring its own
// package-scope `var`. Existing var-as-const sites (cmd/naozhi/setup.go,
// internal/sysession/manager.go, internal/selfupdate, internal/shim/server.go,
// internal/node/relay.go, internal/platform/weixin/api.go,
// internal/server/project_files.go, internal/platform/feishu/feishu.go,
// internal/session/router_core.go, internal/cli/process.go,
// internal/server/wsclient.go, ...) stay where they are for now — moving
// them is mechanical but each one needs its own PR with reviewer attention
// to test overrides.
//
// The point of *this* package is that the next person reaching for
// `var defaultFooTimeout = ...` gets a `go vet`-style nudge from the
// linked TODO comment to add the field here instead.
package timeouts
