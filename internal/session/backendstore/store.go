// Package backendstore is the router's backend table: one row per backend ID
// (wrapper plus per-backend config), the default backend, the router-wide
// cli.model / cli.args base, and the agent-reported model manifests. Every
// row is fixed once New returns and read without a lock; the manifest cache
// carries its own.
//
// The session router reaches it only through methods, so what a backend
// resolves to — which wrapper, under which ID, with which defaults — is
// answered in one place.
package backendstore

import (
	"slices"
	"sync"

	"github.com/naozhi/naozhi/internal/cli"
)

// Runtime is everything the router knows about one backend.
//
// Model and ExtraArgs are the PER-BACKEND overrides (cli.backends[].model /
// .args); empty means "inherit the router-level cli.model / cli.args", and
// session.MergeBackendDefaults applies that precedence. Storing the override
// rather than the resolved value keeps the two tiers distinguishable, which
// the drift view needs.
type Runtime struct {
	// Wrapper is nil for a backend that is configured but has no spawnable
	// wrapper; callers must treat that as "no backend available".
	Wrapper *cli.Wrapper

	Model     string
	ExtraArgs []string
	// Effort has no router-wide base on purpose: the composition root already
	// folded cli.effort in and dropped tier-less backends.
	// docs/rfc/kiro-effort-control.md
	Effort string

	// ConfiguredModels is the operator-declared manifest (cli.backends[].models).
	// docs/rfc/dashboard-model-effort-control.md §4.2.
	ConfiguredModels []string
}

// Config is what New builds a Store from.
type Config struct {
	// Wrapper is the fallback wrapper. With no Runtimes it is also the one
	// backend row, under its BackendID ("claude" when empty).
	Wrapper *cli.Wrapper
	// DefaultBackend names the backend used when a request names none; ""
	// picks the first backend (sorted) that has a wrapper.
	DefaultBackend string
	// Model and ExtraArgs are the router-wide cli.model / cli.args base.
	Model     string
	ExtraArgs []string
	Runtimes  map[string]Runtime
}

// Store is the backend table. A nil *Store answers like an empty one.
type Store struct {
	wrapper        *cli.Wrapper // fallback: the default backend's wrapper, or the first with one
	defaultBackend string
	// ids caches IDs' ordering.
	ids       []string
	model     string
	extraArgs []string
	runtimes  map[string]Runtime
	manifests manifestCache
}

// New normalises cfg into a Store: a lone Wrapper becomes a row, and the
// fallback wrapper and default backend are chosen deterministically.
func New(cfg Config) *Store {
	runtimes := cfg.Runtimes
	defaultBackend := cfg.DefaultBackend
	if len(runtimes) == 0 && cfg.Wrapper != nil {
		id := cfg.Wrapper.BackendID
		if id == "" {
			id = "claude"
		}
		runtimes = map[string]Runtime{id: {Wrapper: cfg.Wrapper}}
		if defaultBackend == "" {
			defaultBackend = id
		}
	}
	fallback := cfg.Wrapper
	if fallback == nil && defaultBackend != "" {
		fallback = runtimes[defaultBackend].Wrapper
	}
	if fallback == nil {
		// Pick deterministically: Go map iteration is randomised, so without
		// sorting a multi-backend deployment with no explicit DefaultBackend
		// would flip its default on every process start. A row from config
		// alone has no wrapper and is not spawnable, so it is skipped.
		for _, id := range sortedKeys(runtimes) {
			if w := runtimes[id].Wrapper; w != nil {
				fallback = w
				if defaultBackend == "" {
					defaultBackend = id
				}
				break
			}
		}
	}
	s := &Store{
		wrapper:        fallback,
		defaultBackend: defaultBackend,
		model:          cfg.Model,
		extraArgs:      cfg.ExtraArgs,
		runtimes:       make(map[string]Runtime, len(runtimes)),
	}
	for id, rt := range runtimes {
		s.runtimes[id] = rt
	}
	s.ids = computeIDs(s.wrapper, s.Wrappers(), s.defaultBackend)
	return s
}

func sortedKeys(m map[string]Runtime) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Config returns the normalised configuration the table holds: New(s.Config())
// builds an identical table (the manifest cache aside, which is not config).
func (s *Store) Config() Config {
	if s == nil {
		return Config{}
	}
	rows := make(map[string]Runtime, len(s.runtimes))
	for id, rt := range s.runtimes {
		rows[id] = rt
	}
	return Config{
		Wrapper:        s.wrapper,
		DefaultBackend: s.defaultBackend,
		Model:          s.model,
		ExtraArgs:      s.extraArgs,
		Runtimes:       rows,
	}
}

// Runtime returns id's row, or the zero Runtime when the backend is unknown.
// A value, not a pointer: every reader wants a snapshot, and the zero value is
// the right answer for a missing backend without a nil check in between.
func (s *Store) Runtime(id string) Runtime {
	if s == nil {
		return Runtime{}
	}
	return s.runtimes[id]
}

// Fallback returns the fallback wrapper, or nil.
func (s *Store) Fallback() *cli.Wrapper {
	if s == nil {
		return nil
	}
	return s.wrapper
}

// DefaultID returns the default backend ID as resolved by New; it is empty
// when New had no backend to choose.
func (s *Store) DefaultID() string {
	if s == nil {
		return ""
	}
	return s.defaultBackend
}

// Default returns the backend used when a request names none: DefaultID, else
// the fallback wrapper's own backend.
func (s *Store) Default() string {
	if d := s.DefaultID(); d != "" {
		return d
	}
	if w := s.Fallback(); w != nil {
		return w.BackendID
	}
	return ""
}

// WrapperFor selects the wrapper for the requested backend ID (empty = the
// default) and returns (wrapper, effectiveID): the requested backend's row,
// else the default backend's, else the fallback wrapper. effectiveID is always
// the returned wrapper's own backend, so a session is stamped with the CLI
// that actually runs it. A nil wrapper means no backend is available.
func (s *Store) WrapperFor(backend string) (*cli.Wrapper, string) {
	if backend != "" {
		if w := s.Runtime(backend).Wrapper; w != nil {
			return w, backend
		}
	}
	if d := s.DefaultID(); d != "" {
		if w := s.Runtime(d).Wrapper; w != nil {
			return w, d
		}
	}
	if w := s.Fallback(); w != nil {
		return w, w.BackendID
	}
	return nil, ""
}

// Wrapper returns the wrapper registered for backend id ("" = the default),
// or nil. For read-only metadata (CLIName, CLIVersion, CLIPath).
func (s *Store) Wrapper(id string) *cli.Wrapper {
	if id == "" {
		id = s.DefaultID()
	}
	return s.Runtime(id).Wrapper
}

// IDs returns the backend IDs the router can spawn against, default first,
// the rest sorted. A fresh copy each call.
func (s *Store) IDs() []string {
	if s == nil {
		return nil
	}
	return slices.Clone(s.ids)
}

// Wrappers returns the id → wrapper view of the rows that have one. Rebuilt
// per call rather than stored, so it cannot drift from the rows.
func (s *Store) Wrappers() map[string]*cli.Wrapper {
	if s == nil || len(s.runtimes) == 0 {
		return nil
	}
	out := make(map[string]*cli.Wrapper, len(s.runtimes))
	for id, rt := range s.runtimes {
		if rt.Wrapper != nil {
			out[id] = rt.Wrapper
		}
	}
	return out
}

// RouterModel and RouterArgs are the router-wide cli.model / cli.args base.
func (s *Store) RouterModel() string {
	if s == nil {
		return ""
	}
	return s.model
}

// RouterArgs is returned without copying; callers that mutate must copy.
func (s *Store) RouterArgs() []string {
	if s == nil {
		return nil
	}
	return s.extraArgs
}

// Manifest returns the cached agent-reported model list for backend id.
func (s *Store) Manifest(id string) []cli.ModelInfo {
	if s == nil {
		return nil
	}
	return s.manifests.get(id)
}

// SetManifest caches backend id's agent-reported model list. It outlives the
// process that reported it.
func (s *Store) SetManifest(id string, models []cli.ModelInfo) {
	if s == nil {
		return
	}
	s.manifests.set(id, models)
}

// computeIDs builds IDs' ordering: default backend first, the rest sorted.
func computeIDs(wrapper *cli.Wrapper, wrappers map[string]*cli.Wrapper, defaultBackend string) []string {
	if len(wrappers) == 0 {
		if wrapper != nil {
			id := wrapper.BackendID
			if id == "" {
				id = "claude"
			}
			return []string{id}
		}
		return nil
	}
	out := make([]string, 0, len(wrappers))
	if defaultBackend != "" {
		if _, ok := wrappers[defaultBackend]; ok {
			out = append(out, defaultBackend)
		}
	}
	rest := make([]string, 0, len(wrappers))
	for id := range wrappers {
		if id == defaultBackend {
			continue
		}
		rest = append(rest, id)
	}
	slices.Sort(rest)
	return append(out, rest...)
}

// manifestCache holds each backend's agent-reported model list. It is the one
// piece of backend state written after New, so it carries its own lock.
type manifestCache struct {
	mu   sync.Mutex
	byID map[string][]cli.ModelInfo
}

func (m *manifestCache) get(id string) []cli.ModelInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byID[id]
}

func (m *manifestCache) set(id string, models []cli.ModelInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byID == nil {
		m.byID = make(map[string][]cli.ModelInfo)
	}
	m.byID[id] = models
}
