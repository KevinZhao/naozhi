package assets

// Provider is the minimal interface a backend implements to expose a
// read-only "installed assets" view to the dashboard (RFC §3.1). The
// implementation is stateless with respect to the environment: every call
// passes Home/RepoRoot explicitly via a request struct, so the single
// Profile.AssetProvider instance can serve per-workspace requests with
// different RepoRoots without caching the wrong one.
//
// Kept to two methods (accept-interfaces idiom). Cache invalidation
// (RFC §3.4 event 1) is NOT part of this interface — it is a concrete method
// on the ccassets implementation, since kiro/codex may have different
// invalidation semantics.
type Provider interface {
	// Scan returns the installed-asset snapshot. By decision D4/D5 the
	// implementation always does a full scan internally (caching the whole
	// Inventory, with Totals aggregated in the same pass); req.Kind only
	// narrows what is RETURNED, it does not change the underlying scan.
	Scan(ScanRequest) (*Inventory, error)
	// ReadRaw returns one asset's raw file bytes, validated against a
	// whitelist the implementation derives on the fly from req + Ref (§5).
	ReadRaw(RawRequest) ([]byte, error)
}

// ScanRequest carries the per-call environment for Scan.
type ScanRequest struct {
	// Home is the resolved Claude config dir (~/.claude), injected by server.
	Home string
	// RepoRoot is the current workspace root for project-level assets and
	// memory. Empty means "skip project-level + memory sources".
	RepoRoot string
	// Kind, when non-empty, narrows the returned Assets to that kind. It does
	// not affect the internal full scan or Totals (D4/D5).
	Kind string
}

// RawRequest carries the per-call environment plus the target Ref for ReadRaw.
type RawRequest struct {
	Home     string
	RepoRoot string
	Ref      Ref
}
