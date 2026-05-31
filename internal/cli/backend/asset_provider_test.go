package backend

import (
	"testing"

	"github.com/naozhi/naozhi/internal/assets"
)

// stubProvider is a no-op assets.Provider for wiring tests.
type stubProvider struct{}

func (stubProvider) Scan(assets.ScanRequest) (*assets.Inventory, error) {
	return &assets.Inventory{}, nil
}
func (stubProvider) ReadRaw(assets.RawRequest) ([]byte, error) { return nil, nil }

func TestAttachAssetProvider(t *testing.T) {
	withCleanRegistry(t, func() {
		Register(sampleProfile("claude"))

		// Before attach: nil.
		if p, _ := Get("claude"); p.AssetProvider != nil {
			t.Fatal("AssetProvider should be nil before attach")
		}

		// Attach to a known id succeeds and is observable via Get/All.
		if ok := AttachAssetProvider("claude", stubProvider{}); !ok {
			t.Fatal("AttachAssetProvider returned false for known id")
		}
		p, _ := Get("claude")
		if p.AssetProvider == nil {
			t.Fatal("AssetProvider nil after attach")
		}

		// Unknown id returns false, no panic.
		if ok := AttachAssetProvider("nope", stubProvider{}); ok {
			t.Fatal("AttachAssetProvider should return false for unknown id")
		}
	})
}
