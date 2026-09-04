package server

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/assets"
	"github.com/naozhi/naozhi/internal/ccassets"
	"github.com/naozhi/naozhi/internal/cli/backend"
	extccassets "github.com/naozhi/naozhi/internal/dashboard/ext/ccassets"
)

// registerAssetBrowserRoutes wires the read-only installed-asset browser
// (docs/rfc/cc-asset-browser.md). The claude provider is attached HERE
// because server is the neutral layer importing both internal/cli/backend
// and internal/ccassets — attaching inside backend would be an import cycle.
// Project-level + memory sources are gated behind a repoRoot that is always
// "" for now (RFC §9.3), so only user-level + plugin assets surface.
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
			func(*http.Request) string { return "" }, // project scope deferred (RFC §9.3)
			newIPLimiterWithProxy(extccassets.AssetsLimiterRate, extccassets.AssetsLimiterBurst, s.auth.TrustedProxy),
		)
	}
	s.mux.HandleFunc("GET /api/cc/assets", auth(s.ccAssetsH.HandleList))
	s.mux.HandleFunc("GET /api/cc/assets/raw", auth(s.ccAssetsH.HandleRaw))
}
