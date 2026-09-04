// registry.go provides the generic type-safe registration idiom (Registry[T])
// with a duplicate-key panic, so double-wireup surfaces at startup rather than
// at first runtime use (#1058).
package wireup

import (
	"fmt"
	"sort"
	"sync"
)

// Registry is a concurrency-safe, type-safe name→value table for boot-time
// subsystem registration. Zero value is NOT usable — construct via NewRegistry.
type Registry[T any] struct {
	// kind labels the registry in panic/audit messages ("backend", "platform").
	kind string
	mu   sync.RWMutex
	m    map[string]T
}

// NewRegistry constructs an empty Registry; kind is used only in panic text.
func NewRegistry[T any](kind string) *Registry[T] {
	return &Registry[T]{kind: kind, m: make(map[string]T)}
}

// Register adds value under name. It panics on a duplicate or empty name —
// both are wiring bugs that must fail loudly at boot, not shadow silently.
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

// Names returns the registered names sorted, so audit output is deterministic.
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
