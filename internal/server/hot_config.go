package server

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/imauth"
)

// Hot reload (docs/rfc/config-hot-reload.md §3.2): the runtime half. The
// composition root loads and diffs the file; this file applies the hot
// sections to the dispatcher and keeps /health's fingerprint current.

// HotConfig carries the derived values of the hot-reloadable sections.
type HotConfig struct {
	Access    *imauth.Policy
	RateLimit dispatch.RateLimit
	// Sections names the yaml sections to apply ("im_access",
	// "im_rate_limit"); the others keep their running value, so a reload
	// that leaves im_rate_limit alone does not refill every sender's bucket.
	Sections []string
}

// ApplyHotConfig swaps the dispatcher's access policy and sender rate limit
// for h's, each only when h.Sections lists it. Safe before Start and against
// concurrent IM traffic: each target is an atomic pointer the next message
// reads.
func (s *Server) ApplyHotConfig(h HotConfig) {
	if s.dispatcher == nil {
		return
	}
	if slices.Contains(h.Sections, "im_access") {
		s.dispatcher.SetAccessPolicy(h.Access)
	}
	if slices.Contains(h.Sections, "im_rate_limit") {
		s.dispatcher.SetRateLimit(h.RateLimit)
	}
}

// ConfigReloadFunc re-reads the config file and applies its hot sections;
// the composition root supplies it (cmd/naozhi/reload.go).
type ConfigReloadFunc func(ctx context.Context) (config.ReloadResult, error)

// ConfigFingerprint is the loaded config's sha256 / load time as /health
// reports them, plus the sections the last reload could not apply. Zero
// value = unknown.
type ConfigFingerprint struct {
	mu              sync.Mutex
	sha256          string
	loadedAt        time.Time
	restartRequired []string
}

// NewConfigFingerprint seeds a fingerprint with the startup values.
func NewConfigFingerprint(sha string, at time.Time) *ConfigFingerprint {
	return &ConfigFingerprint{sha256: sha, loadedAt: at}
}

// Set records a reloaded file. config_sha256 means the process runs those
// bytes in full (#2538), so sha and at are taken only when restartRequired
// is empty; otherwise the previous fingerprint stays and the sections are
// what /health lists as still needing a restart.
func (f *ConfigFingerprint) Set(sha string, at time.Time, restartRequired []string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restartRequired = slices.Clone(restartRequired)
	if len(restartRequired) == 0 {
		f.sha256, f.loadedAt = sha, at
	}
}

// Get returns the current fingerprint; a nil receiver reads as unknown.
func (f *ConfigFingerprint) Get() (sha string, at time.Time, restartRequired []string) {
	if f == nil {
		return "", time.Time{}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sha256, f.loadedAt, slices.Clone(f.restartRequired)
}
