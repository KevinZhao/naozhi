// Package registry is the canonical home for naozhi plugin / extension
// registries. [Typed] is a small generic registry with three properties:
// its constructor takes a name so errors are self-describing, Register
// returns an error instead of panicking so the call site decides whether a
// duplicate is fatal, and iteration order is deterministic (sorted by key).
//
// New plugin registries MUST use [Typed]; package-level init() MUST NOT be
// used for plugin registration. Plugins → [Typed.Register] from a
// constructor or main.go; test seams → struct field + constructor parameter;
// startup self-checks → an explicit step in main (#660).
package registry

import (
	"fmt"
	"sort"
	"sync"
)

// Typed is a thread-safe generic registry of named values of type T for
// plugin-style closed sets (daemons, backends, channel adapters). The zero
// value is NOT ready for use; construct with [New].
type Typed[T any] struct {
	name string

	mu      sync.RWMutex
	entries map[string]T
}

// New returns an empty registry tagged with name, which is embedded in error messages.
func New[T any](name string) *Typed[T] {
	return &Typed[T]{
		name:    name,
		entries: make(map[string]T),
	}
}

// Register adds value under key. A duplicate key returns an error and leaves
// the existing entry untouched, so the caller decides whether that is fatal
// (startup wiring) or ignorable (test re-runs). An empty key is rejected.
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

// Lookup returns the value registered under key and whether it exists.
func (r *Typed[T]) Lookup(key string) (T, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.entries[key]
	return v, ok
}

// Names returns a sorted copy of all registered keys; sorting keeps
// "registry contains exactly these names" assertions deterministic.
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

// Len returns the number of registered entries.
func (r *Typed[T]) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
