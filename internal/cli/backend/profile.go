// Package backend collects per-CLI-backend metadata (display name, default
// binary, protocol constructor, in-process detection predicate, required
// reverse-node capabilities) into a single Profile registry.
//
// Other modules that previously hard-coded `switch backend.ID` knowledge —
// wrapper.go, detect.go, cmd/naozhi/main.go, discovery/proc_*.go,
// server/server.go — are intended to call backend.Get(id) / backend.All()
// instead. This Sprint (0b) only introduces the package; the actual
// 散点收敛 happens in Sprint 1b.
//
// Registration is explicit: callers must invoke RegisterDefaults() (typically
// from main early in startup). init() self-registration is deliberately
// avoided so that tests can wire up only the profiles they need without
// hidden global state.
package backend

import (
	"fmt"
	"sync"

	"github.com/naozhi/naozhi/internal/cli"
)

// Profile is the complete capability description of a single CLI backend.
// All fields are immutable once registered. Adding a backend means writing
// a new profile_<id>.go file with a constructor and adding it to
// RegisterDefaults — no consumer code needs to learn about the new ID.
type Profile struct {
	// ID is the canonical backend identifier ("claude", "kiro", ...).
	// Stable, lowercase, ascii. Used as registry key and config token.
	ID string

	// DisplayName is the human-readable label shown in dashboard chips,
	// log lines, etc. ("claude-code", "kiro").
	DisplayName string

	// DefaultBinary is the executable name to look for on PATH when
	// `cli.path` is not configured. Resolved via exec.LookPath at
	// detection time.
	DefaultBinary string

	// DefaultTag is the default reply-prefix tag ("cc", "kiro", "gem")
	// applied to outbound messages when per-session ReplyTag is unset.
	// Config can still override.
	DefaultTag string

	// NewProtocol constructs a fresh cli.Protocol implementation for this
	// backend. Called once per session spawn. Receivers should not retain
	// the ProtocolDeps after returning; they are scoped to the call.
	NewProtocol func(ProtocolDeps) cli.Protocol

	// DetectInProc inspects an OS process command-line string and reports
	// whether the process belongs to this backend. Used by
	// internal/discovery/proc_*.go to label running CLIs. Implementations
	// must be cheap and side-effect free.
	DetectInProc func(cmdline string) bool

	// RequiredNodeCaps lists the reverse-node capability strings a child
	// node must advertise before it is allowed to host this backend's
	// sessions (e.g. "acp" for kiro). nil/empty means "no special cap
	// required" (claude is in this bucket).
	RequiredNodeCaps []string
}

// ProtocolDeps bundles dependencies needed to construct certain protocols.
// Most fields are claude-specific (settings file plumbing); ACP profiles
// can ignore them.
type ProtocolDeps struct {
	// SettingsFile is the path to a filtered claude settings.json override
	// (with hooks calling back into naozhi stripped). Empty for protocols
	// that don't honor it.
	SettingsFile string

	// RefreshSettings, when non-nil, is invoked at the start of every
	// BuildArgs call. Returning a non-empty path swaps SettingsFile for
	// the next spawn; returning "" means "keep the prior value, refresh
	// transiently failed" — the caller must NOT clear an existing path
	// just because refresh failed (Bedrock auth would break).
	RefreshSettings func() string
}

// registryEntry pairs a Profile with its registration order so All()
// can return profiles in the order Register was called.
type registryEntry struct {
	order   int
	profile Profile
}

var (
	registryMu sync.RWMutex
	registry   = map[string]registryEntry{}
	nextOrder  int
)

// Register adds a Profile to the registry. Panics on duplicate ID — there
// is no legitimate reason to register the same backend twice, and silent
// last-write-wins would mask programmer error.
func Register(p Profile) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[p.ID]; exists {
		panic(fmt.Sprintf("backend: duplicate registration of %q", p.ID))
	}
	registry[p.ID] = registryEntry{order: nextOrder, profile: p}
	nextOrder++
}

// Get returns the Profile registered under id and whether it exists.
// Callers that want a hard failure on missing IDs should use MustGet.
func Get(id string) (Profile, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	e, ok := registry[id]
	if !ok {
		return Profile{}, false
	}
	return e.profile, true
}

// MustGet returns the Profile registered under id or panics if absent.
// Use only for IDs that must be present at this point in startup
// (e.g. main.go after RegisterDefaults). Library code should prefer Get.
func MustGet(id string) Profile {
	p, ok := Get(id)
	if !ok {
		panic(fmt.Sprintf("backend: unknown id %q", id))
	}
	return p
}

// All returns every registered Profile in registration order. The slice
// is a fresh copy; mutating it has no effect on the registry.
func All() []Profile {
	registryMu.RLock()
	defer registryMu.RUnlock()
	entries := make([]registryEntry, 0, len(registry))
	for _, e := range registry {
		entries = append(entries, e)
	}
	// Sort by registration order so deterministic iteration is preserved
	// regardless of map iteration order. Insertion sort is fine; the
	// registry will hold a handful of entries at most.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j-1].order > entries[j].order; j-- {
			entries[j-1], entries[j] = entries[j], entries[j-1]
		}
	}
	out := make([]Profile, len(entries))
	for i, e := range entries {
		out[i] = e.profile
	}
	return out
}

// RegisterDefaults registers the built-in profiles (claude, kiro). Must be
// called once during startup before any consumer touches the registry.
// Idempotency is intentionally NOT supported: calling twice will panic via
// Register's duplicate check, surfacing accidental double-init.
func RegisterDefaults() {
	Register(claudeProfile())
	Register(kiroProfile())
}

// reset is a test-only helper that clears the registry. Lives in the main
// file (not _test.go) so external test packages can also use it via
// reflection if ever needed; it is unexported so production code cannot
// accidentally invoke it.
func reset() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]registryEntry{}
	nextOrder = 0
}
