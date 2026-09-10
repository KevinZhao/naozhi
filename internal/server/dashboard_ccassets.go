package server

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/assets"
	"github.com/naozhi/naozhi/internal/ccassets"
	"github.com/naozhi/naozhi/internal/cli/backend"
	extccassets "github.com/naozhi/naozhi/internal/dashboard/ext/ccassets"
)

// buildAssetBrowser constructs the read-only installed-asset browser
// (docs/rfc/cc-asset-browser.md). The claude provider is attached HERE because
// server is the neutral layer importing both internal/cli/backend and
// internal/ccassets — attaching inside backend would be an import cycle.
// Project-level + memory sources are gated behind a repoRoot that is always ""
// for now (RFC §9.3), so only user-level + plugin assets surface.
//
// Was registerAssetBrowserRoutes, which constructed the handler lazily on its
// way to registering two routes. #2554 moved the patterns into the ccassets
// package's own Routes(), so what is left here is construction — and it belongs
// with the rest of it in buildDashboard.
func (s *Server) buildAssetBrowser() *extccassets.Handler {
	backend.AttachAssetProvider("claude", ccassets.NewClaudeProvider())
	providers := map[string]assets.Provider{}
	for _, p := range backend.All() {
		if p.AssetProvider != nil {
			providers[p.ID] = p.AssetProvider
		}
	}
	return extccassets.New(
		providers,
		resolveClaudeDir(),
		func(*http.Request) string { return "" }, // project scope deferred (RFC §9.3)
		newIPLimiterWithProxy(extccassets.AssetsLimiterRate, extccassets.AssetsLimiterBurst, s.auth.TrustedProxy),
	)
}
