package assets

// Provider exposes a read-only "installed assets" view to the dashboard.
// Implementations are stateless w.r.t. the environment: Home/RepoRoot arrive
// per call, so one Profile.AssetProvider serves per-workspace requests.
type Provider interface {
	// Scan returns the snapshot; it always scans fully, req.Kind only narrows the result.
	Scan(ScanRequest) (*Inventory, error)
	// ReadRaw returns one asset's raw bytes, validated against a whitelist from req + Ref.
	ReadRaw(RawRequest) ([]byte, error)
}

// ScanRequest carries the per-call environment for Scan.
type ScanRequest struct {
	// Home is the resolved Claude config dir (~/.claude).
	Home string
	// RepoRoot is the workspace root; empty skips project-level + memory sources.
	RepoRoot string
	// Kind, when non-empty, narrows the returned Assets (not Totals).
	Kind string
}

// RawRequest carries the per-call environment plus the target Ref for ReadRaw.
type RawRequest struct {
	Home     string
	RepoRoot string
	Ref      Ref
}
