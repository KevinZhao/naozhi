// Package backend collects per-CLI-backend metadata (display name, default
// binary, protocol constructor, in-process detection predicate, required
// reverse-node capabilities) into a single Profile registry. Consumers call
// backend.Get(id) / backend.All() instead of switching on backend IDs.
//
// Registration is explicit: callers invoke RegisterDefaults() (typically from
// main). init() self-registration is avoided so tests can wire up only the
// profiles they need without hidden global state.
package backend

import (
	"fmt"
	"sync"

	"github.com/naozhi/naozhi/internal/assets"
	"github.com/naozhi/naozhi/internal/cli"
)

// Profile is the complete capability description of a single CLI backend.
// All fields are immutable once registered. Adding a backend means writing
// a new profile_<id>.go constructor and adding it to RegisterDefaults.
type Profile struct {
	// ID is the canonical backend identifier ("claude", "kiro", ...):
	// stable, lowercase ascii; registry key and config token.
	ID string

	// DisplayName is the human-readable label ("claude-code", "kiro").
	DisplayName string

	// DefaultBinary is the executable looked up on PATH when `cli.path` is unset.
	DefaultBinary string

	// DefaultTag is the reply-prefix tag ("cc", "kiro", "gem") when per-session
	// ReplyTag is unset. Config can override.
	DefaultTag string

	// ChipColor is the dashboard chip background (CSS color string). Empty
	// means the dashboard default token; CLIBackendConfig can override.
	ChipColor string

	// NewProtocol constructs a fresh cli.Protocol for this backend, once per
	// session spawn. Receivers must not retain the ProtocolDeps.
	NewProtocol func(ProtocolDeps) cli.Protocol

	// DetectInProc reports whether an OS process command-line belongs to this
	// backend (internal/discovery). Must be cheap and side-effect free.
	DetectInProc func(cmdline string) bool

	// RequiredNodeCaps lists reverse-node capability strings a child node must
	// advertise to host this backend's sessions (e.g. "acp"). nil = none.
	RequiredNodeCaps []string

	// HistoryDir is where this backend persists session transcripts that
	// internal/history/* read. Stored with a leading "~/" so doctor output can
	// display it verbatim; consumers expand "~/" via os.UserHomeDir. Empty
	// means no on-disk history — check before joining paths.
	HistoryDir string

	// CostUnit labels cumulative cost cells: "USD" (claude), "credits" (kiro).
	// Empty means no cost concept — the dashboard hides the cell.
	CostUnit string

	// Features is the user-facing capability map the dashboard reads to gray
	// out controls. Distinct from protocol-level cli.Caps. Keys the frontend
	// hard-codes: "askuser", "passthrough", "embedded_context", "image_input",
	// "audio_input", "mcp_http", "mcp_sse". Missing key == false. Adding a
	// feature: extend dashboard.js featureForCurrent + every supporting Profile.
	Features map[string]bool

	// AssetProvider, when non-nil, exposes this backend's installed assets to
	// the dashboard asset browser read-only. nil = no asset view. Injected
	// post-registration via AttachAssetProvider from the server layer (this
	// package must not import the file-scanning ccassets package).
	AssetProvider assets.Provider
}

// ProtocolDeps bundles dependencies needed to construct protocols. Currently
// empty; retained as the NewProtocol parameter so future per-spawn
// dependencies do not change every backend's factory signature.
type ProtocolDeps struct{}

// registryEntry pairs a Profile with its registration order so All()
// returns profiles in Register order.
type registryEntry struct {
	order   int
	profile Profile
}

var (
	registryMu sync.RWMutex
	registry   = map[string]registryEntry{}
	nextOrder  int

	// defaultsOnce serialises EnsureDefaults; tests reset it via withCleanRegistry.
	defaultsOnce sync.Once
)

// Register adds a Profile to the registry. Panics on duplicate ID: silent
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
func Get(id string) (Profile, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	e, ok := registry[id]
	if !ok {
		return Profile{}, false
	}
	return e.profile, true
}

// AttachAssetProvider sets the AssetProvider on an already-registered Profile;
// false if id is unknown. A post-registration mutator because this package
// must NOT import ccassets (import cycle); the server wiring layer that
// legitimately imports both calls this after RegisterDefaults.
func AttachAssetProvider(id string, p assets.Provider) bool {
	registryMu.Lock()
	defer registryMu.Unlock()
	e, ok := registry[id]
	if !ok {
		return false
	}
	e.profile.AssetProvider = p
	registry[id] = e
	return true
}

// All returns every registered Profile in registration order as a fresh copy.
func All() []Profile {
	registryMu.RLock()
	defer registryMu.RUnlock()
	entries := make([]registryEntry, 0, len(registry))
	for _, e := range registry {
		entries = append(entries, e)
	}
	// Insertion sort by registration order; a handful of entries at most.
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

// RegisterDefaults registers the built-in profiles (claude, kiro, codex).
// Call once during startup; a second call panics via Register's duplicate
// check. Library callers that may run before main should use EnsureDefaults.
func RegisterDefaults() {
	Register(claudeProfile())
	Register(kiroProfile())
	Register(codexProfile())
}

// EnsureDefaults is the concurrent-safe, idempotent counterpart to
// RegisterDefaults for helpers that may execute before main runs. A panic in
// RegisterDefaults deliberately propagates: recovering would mask a
// partial-registration race.
func EnsureDefaults() {
	defaultsOnce.Do(RegisterDefaults)
}
