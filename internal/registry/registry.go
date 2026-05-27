// Package registry is the canonical home for naozhi plugin / extension
// registries.
//
// # Why this package exists (R247-ARCH-17 / R247-ARCH-21, #660)
//
// A code review found four different registry models scattered across
// the codebase, each with subtly different semantics:
//
//   - sysession.builtinDaemons     — package-scope `var` slice (factory list).
//   - cli/backend Profile lookup    — package-scope `var` map.
//   - history blank imports         — `import _ "..."` triggering
//     init()-side-effect registration into a shared map.
//   - wireup explicit list          — main.go enumerates everything by hand.
//
// The R247 review also flagged 4+ `init()` sites that conflate three
// distinct semantics: plugin registration, test-only seam wiring, and
// startup self-checks. Mixing those in a single init() makes test
// builds flaky (registration order changes when a test imports a
// helper) and makes the "what runs at startup" answer non-discoverable
// without grep.
//
// # The minimum-viable shape (this commit)
//
// We expose [Typed] — a tiny generic registry — with three properties
// the existing models lack:
//
//  1. Constructor takes the registry name so error messages are
//     self-describing ("registry plugin <name> already exists" beats
//     "duplicate key").
//  2. Registration returns an error rather than panicking so the call
//     site decides whether a duplicate is fatal (production) or
//     ignorable (test re-imports).
//  3. Iteration order is deterministic (sorted by key) so a snapshot
//     of registered names compares cleanly across runs.
//
// # Migration policy
//
// New plugin registries MUST use [Typed[T]] from this package. Existing
// registries (sysession.builtinDaemons, cli/backend Profile, history
// blank-imports, wireup) stay where they are; each migration is
// mechanical but needs reviewer attention to startup order. The point
// of *this* package is that the next person reaching for "another
// init()-side-effect registration" gets a single canonical alternative.
//
// As a hard rule going forward: package-level init() MUST NOT be used
// for plugin registration. Plugins → [Typed.Register] from a
// constructor or main.go. Test seams → struct field + constructor
// parameter. Startup self-checks → an explicit step in main.
package registry

import (
	"fmt"
	"sort"
	"sync"
)

// Typed is a thread-safe generic registry of named values of type T.
// It is intended for plugin-style "the application supports a closed
// set of these, register one or more of them at construction time"
// use cases — daemons, backends, channel adapters, etc.
//
// The zero value is NOT ready for use; callers must use [New] to
// construct an instance with the registry name embedded for error
// messages.
type Typed[T any] struct {
	name string

	mu      sync.RWMutex
	entries map[string]T
}

// New returns a fresh empty registry tagged with name. The name is
// embedded in error messages so a caller looking at an error log can
// tell which registry rejected the duplicate without re-deriving from
// the call stack.
func New[T any](name string) *Typed[T] {
	return &Typed[T]{
		name:    name,
		entries: make(map[string]T),
	}
}

// Register adds value under key. If key is already registered, returns
// an error and leaves the existing entry untouched (no last-write-wins
// surprise). Returning an error rather than panicking lets the caller
// decide: a startup wiring step should treat the error as fatal, while
// a test-helper that re-runs setup may want to ignore it.
//
// An empty key is rejected — registering "" hides bugs where the
// caller forgot to compute the name.
func (r *Typed[T]) Register(key string, value T) error {
	if key == "" {
		return fmt.Errorf("registry %q: empty key", r.name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[key]; exists {
		return fmt.Errorf("registry %q: duplicate entry %q", r.name, key)
	}
	r.entries[key] = value
	return nil
}

// Lookup returns the value registered under key and a boolean
// indicating whether the entry exists. The two-return shape mirrors
// the Go map idiom so existing code patterns translate directly.
func (r *Typed[T]) Lookup(key string) (T, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.entries[key]
	return v, ok
}

// Names returns a sorted snapshot of all registered keys. Sorting is
// load-bearing for tests that assert "the registry contains exactly
// these names" — without it, map iteration order would make every
// such test flaky on a fresh Go release.
//
// The slice is a copy; the caller may mutate it without affecting the
// registry.
func (r *Typed[T]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for k := range r.entries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Len returns the number of registered entries. Cheaper than
// len(Names()) when the caller only needs the count for a metric or a
// guard expression.
func (r *Typed[T]) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
