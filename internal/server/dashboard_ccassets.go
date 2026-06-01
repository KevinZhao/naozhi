package server

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/assets"
	"github.com/naozhi/naozhi/internal/ccassets"
	"github.com/naozhi/naozhi/internal/cli/backend"
	extccassets "github.com/naozhi/naozhi/internal/dashboard/ext/ccassets"
)

// registerAssetBrowserRoutes wires the read-only installed-asset browser
// (docs/rfc/cc-asset-browser.md). Split out of registerDashboard to keep that
// file from growing (mirrors registerScratchRoutes / static_assets.go).
//
// The claude provider is attached to the backend registry HERE — server is the
// neutral top layer that legitimately imports both internal/cli/backend and
// internal/ccassets, so injecting via backend.AttachAssetProvider avoids the
// backend→ccassets import cycle the RFC's B2 review flagged (§3.0). The handler
// then collects every backend that exposes a provider, so kiro/codex appear
// automatically once they register one.
//
// P0 scope: project-level + memory sources are gated behind a repoRoot that is
// always "" for now (RFC §9.3 deferred), so only user-level + plugin assets
// surface until the workspace-resolution contract is finalised.
func (s *Server) registerAssetBrowserRoutes(auth func(http.HandlerFunc) http.HandlerFunc) {
	if s.ccAssetsH == nil {
		backend.AttachAssetProvider("claude", ccassets.NewClaudeProvider())
		providers := map[string]assets.Provider{}
		for _, p := range backend.All() {
			if p.AssetProvider != nil {
				providers[p.ID] = p.AssetProvider
			}
		}
		s.ccAssetsH = extccassets.New(
			providers,
			resolveClaudeDir(),
			func(*http.Request) string { return "" }, // P0: project scope deferred (RFC §9.3)
			newIPLimiterWithProxy(extccassets.AssetsLimiterRate, extccassets.AssetsLimiterBurst, s.auth.TrustedProxy),
		)
	}
	s.mux.HandleFunc("GET /api/cc/assets", auth(s.ccAssetsH.HandleList))
	s.mux.HandleFunc("GET /api/cc/assets/raw", auth(s.ccAssetsH.HandleRaw))
}
