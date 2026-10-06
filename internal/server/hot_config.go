package server

import (
	"context"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/imauth"
)

// Hot reload (docs/rfc/config-hot-reload.md §3.2): the runtime half. The
// composition root loads and diffs the file; this file applies the hot
// sections to the dispatcher and keeps /health's fingerprint current.

// HotConfig carries the derived values of the hot-reloadable sections.
type HotConfig struct {
	Access   *imauth.Policy
	IMLimits IMLimitsOptions
}

// ApplyHotConfig swaps the dispatcher's access policy and usage gates for
// h's. Safe before Start and against concurrent IM traffic: each target is an
// atomic pointer the next message reads.
func (s *Server) ApplyHotConfig(h HotConfig) {
	if s.dispatcher == nil {
		return
	}
	s.dispatcher.SetAccessPolicy(h.Access)
	s.dispatcher.SetBudgetGate(s.buildBudgetGate(h.IMLimits.Budget))
	s.dispatcher.SetUserLimiter(buildUserLimiter(h.IMLimits))
}

// ConfigReloadFunc re-reads the config file and applies its hot sections;
// the composition root supplies it (cmd/naozhi/reload.go).
type ConfigReloadFunc func(ctx context.Context) (config.ReloadResult, error)

// ConfigFingerprint is the loaded config's sha256 / load time as /health
// reports them, updated by every successful reload. Zero value = unknown.
type ConfigFingerprint struct {
	mu       sync.Mutex
	sha256   string
	loadedAt time.Time
}

// NewConfigFingerprint seeds a fingerprint with the startup values.
func NewConfigFingerprint(sha string, at time.Time) *ConfigFingerprint {
	return &ConfigFingerprint{sha256: sha, loadedAt: at}
}

// Set records a newly loaded file.
func (f *ConfigFingerprint) Set(sha string, at time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sha256, f.loadedAt = sha, at
}

// Get returns the current fingerprint; a nil receiver reads as unknown.
func (f *ConfigFingerprint) Get() (sha string, at time.Time) {
	if f == nil {
		return "", time.Time{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sha256, f.loadedAt
}
