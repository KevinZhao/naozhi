// Package wireup — registry.go provides a single generic registration
// idiom (Registry[T]) so cron daemons, platforms, and backends can all
// register through the same type-safe surface instead of each subsystem
// reinventing its own register-call + duplicate-guard.
//
// R244-ARCH-4 (#1058): the blank-import wireup pattern (history_backends.go)
// only covers history backends; CLI backends use an explicit RegisterDefaults
// call (backends.go), and cron/sysession/platforms each have bespoke
// construction-time wiring (schedulers.go). The proposal is a unified
// Registry[T] that every sub-system registers through with one idiom.
//
// This file introduces that idiom as a leaf utility with no external
// dependencies so it can be adopted incrementally: a subsystem migrates by
// declaring a package-level Registry[T] and calling Register at init() or
// boot. The duplicate-key panic matches the existing semantics of
// cli.RegisterHistoryFactory and backend.Register, so accidental
// double-wireup keeps surfacing at startup rather than at first runtime use.
package wireup

import (
	"fmt"
	"sort"
	"sync"
)

// Registry is a concurrency-safe, type-safe name→value table for boot-time
// subsystem registration. T is the registered value type (e.g. a factory
// func, a platform constructor, a daemon descriptor).
//
// Zero value is NOT ready for use — construct via NewRegistry so the kind
// label (used in panic messages) and the backing map are initialised.
type Registry[T any] struct {
	// kind labels the registry in panic/audit messages ("backend",
	// "platform", "cron-daemon"); makes a duplicate-registration panic
	// self-describing without the caller threading context.
	kind string
	mu   sync.RWMutex
	m    map[string]T
}

// NewRegistry constructs an empty Registry. kind is a short human label
// used only in panic/error text (e.g. "platform").
func NewRegistry[T any](kind string) *Registry[T] {
	return &Registry[T]{kind: kind, m: make(map[string]T)}
}

// Register adds value under name. It panics on a duplicate name or an
// empty name — both are wiring bugs that must fail loudly at boot rather
// than silently shadowing an earlier registration or registering an
// unaddressable entry. The panic mirrors the existing duplicate-ID guards
// in cli.RegisterHistoryFactory / backend.Register so the unified idiom
// preserves the "fail at startup" contract operators already rely on.
func (r *Registry[T]) Register(name string, value T) {
	if name == "" {
		panic(fmt.Sprintf("wireup: empty %s registration name", r.kind))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.m[name]; dup {
		panic(fmt.Sprintf("wireup: duplicate %s registration %q", r.kind, name))
	}
	r.m[name] = value
}

// Get returns the registered value for name and whether it was present.
func (r *Registry[T]) Get(name string) (T, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.m[name]
	return v, ok
}

// Names returns the registered names in sorted order. Sorted (not map
// order) so audit output / startup logs are deterministic across runs —
// the whole point of a unified registry is an inspectable "what is wired"
// list, which a nondeterministic order would undermine.
func (r *Registry[T]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.m))
	for name := range r.m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Len reports the number of registered entries.
func (r *Registry[T]) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.m)
}
