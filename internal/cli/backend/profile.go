// Package backend collects per-CLI-backend metadata (display name, default
// binary, protocol constructor, in-process detection predicate, required
// reverse-node capabilities) into a single Profile registry.
//
// Other modules that previously hard-coded `switch backend.ID` knowledge —
// wrapper.go, detect.go, cmd/naozhi/main.go, discovery/proc_*.go,
// server/server.go — should call backend.Get(id) / backend.All() instead.
// Migration is incremental: a few residual switch statements remain in cli/
// and session/ and are tracked in docs/TODO.md under the backend-profile
// consolidation theme.
//
// Registration is explicit: callers must invoke RegisterDefaults() (typically
// from main early in startup). init() self-registration is deliberately
// avoided so that tests can wire up only the profiles they need without
// hidden global state.
package backend

import (
	"fmt"
	"sync"

	"github.com/naozhi/naozhi/internal/assets"
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

	// ChipColor is the dashboard chip background color for sessions on this
	// backend. CSS color string ("#7c5cff" / "var(--nz-accent)" etc.).
	// Empty means "use the dashboard's default token", and CLIBackendConfig
	// can override per-deployment. Multi-Backend RFC §8.4.
	ChipColor string

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

	// HistoryDir is the on-disk directory where this backend persists
	// session transcripts that internal/history/* sources read.
	// Convention: stored with a leading "~/" so doctor / debug output
	// can display the user-relative path verbatim. Consumers that need
	// an absolute path must expand "~/" via os.UserHomeDir themselves.
	// Empty string means "this backend has no on-disk history" — code
	// composing paths must check before joining.
	//
	// Centralizing this on Profile (instead of a private
	// `switch backend.ID` in cmd/naozhi/doctor.go) closes the
	// compile-safety hole flagged in PR #117 review: adding a third
	// backend now requires only a Profile entry, not an edit to a
	// distant switch statement that the new-backend author may not
	// know about.
	HistoryDir string

	// CostUnit is the dashboard-facing label for cumulative cost cells:
	// "USD" for claude (Process.TotalCost reports dollars), "credits" for
	// kiro (per-turn metering accrues in ACP credit units). Empty means
	// "this backend has no cost concept" — the dashboard hides the cell.
	//
	// Centralising on Profile lets session.costUnitForBackend look the
	// value up via backend.Get instead of maintaining its own switch
	// (R225-CR-4 / R224-ARCH-1). Adding a new backend with a non-empty
	// cost surface only requires populating this field.
	CostUnit string

	// Features is the user-facing capability map the dashboard reads to
	// decide which UI controls to gray out (RFC §8.2). Distinct from the
	// protocol-level cli.Caps (which is plumbed via Protocol.Capabilities)
	// because dashboard cares about user features, not wire protocol bits.
	//
	// Keys are stable strings the frontend hard-codes:
	//   - "askuser"           — AskUserQuestion 卡片支持
	//   - "passthrough"       — /urgent + 多消息并发
	//   - "embedded_context"  — @file mention
	//   - "image_input"       — 图片上传
	//   - "audio_input"       — 音频直接喂（不是先转写再喂）
	//   - "mcp_http"          — HTTP MCP 服务器
	//   - "mcp_sse"           — SSE MCP 服务器
	//
	// Missing key == false. Adding a new feature: extend the keys list in
	// dashboard.js featureForCurrent + every Profile that supports it.
	Features map[string]bool

	// AssetProvider, when non-nil, exposes this backend's installed assets
	// (skills/plugins/agents/...) to the dashboard asset browser read-only.
	// The on-disk layout is fully encapsulated in the implementation —
	// Profile stays a capability description, unaware of any backend's
	// directory shape. nil = no asset view (dashboard hides the entry).
	// Injected post-registration via AttachAssetProvider from the neutral
	// server layer (the lightweight backend package must not import the
	// file-scanning ccassets package). RFC docs/rfc/cc-asset-browser.md §3.1.
	AssetProvider assets.Provider
}

// ProtocolDeps bundles dependencies needed to construct certain protocols.
//
// It is currently empty: the claude backend used to carry a filtered
// settings-override file path here, but PR1 of
// docs/rfc/direct-user-settings.md switched claude to `--setting-sources user`
// (cc reads ~/.claude/settings.json directly), removing the override plumbing.
// The struct is retained as the NewProtocol parameter so adding future
// per-spawn dependencies does not change every backend's factory signature.
type ProtocolDeps struct{}

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

	// defaultsOnce serialises EnsureDefaults across goroutines. Safe to
	// reset under tests via withCleanRegistry (profile_test.go) so a
	// fresh test can re-bootstrap deterministically.
	defaultsOnce sync.Once
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
// Callers that want a hard failure on missing IDs should check ok and
// fail-fast at the call site (e.g. log.Fatal, t.Fatal, panic).
func Get(id string) (Profile, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	e, ok := registry[id]
	if !ok {
		return Profile{}, false
	}
	return e.profile, true
}

// AttachAssetProvider sets the AssetProvider on an already-registered Profile.
// Returns false if id is unknown. Deliberate post-registration mutator: the
// lightweight backend package must NOT import the file-scanning ccassets
// package (that would re-create an import cycle). A neutral top-level layer
// (server wiring) that legitimately imports both calls this after
// RegisterDefaults. RFC docs/rfc/cc-asset-browser.md §3.0/§3.1.
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
//
// For library callers that may run before main has bootstrapped (doctor
// helpers, ad-hoc test fixtures), prefer EnsureDefaults — it is safe under
// concurrent use AND idempotent.
func RegisterDefaults() {
	Register(claudeProfile())
	Register(kiroProfile())
}

// EnsureDefaults is the concurrent-safe, idempotent counterpart to
// RegisterDefaults. The first call registers the built-in profiles via
// sync.Once; every subsequent call is a no-op. Use this from helpers
// that may execute before main runs (e.g. cmd/naozhi/doctor.go's
// historyDirForBackend) or from concurrent goroutines that all need
// "the registry is initialised" without coordinating.
//
// Deliberate non-recover behaviour: if RegisterDefaults panics for a
// real reason (e.g. duplicate registration introduced by a programmer
// error), the panic propagates out of sync.Once and surfaces — as it
// should. Earlier iterations wrapped a recover() around RegisterDefaults
// here, but that masked partial-registration races (review of PR #122)
// where a panic mid-sequence could leave the registry with claude but
// no kiro and silently swallow it.
func EnsureDefaults() {
	defaultsOnce.Do(RegisterDefaults)
}
