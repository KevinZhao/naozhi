package assets

import "errors"

// Sentinel errors a Provider.ReadRaw may return. They live in this leaf
// package so both the provider implementation (internal/ccassets) and the
// HTTP handler (internal/dashboard/ext/ccassets) can reference them via
// errors.Is without importing each other (RFC §3.0).
var (
	// ErrNotFound covers both "no such asset" and "path escaped the allowed
	// root". They are deliberately merged so the API does not leak whether a
	// traversal target exists — the handler maps both to HTTP 404.
	ErrNotFound = errors.New("assets: not found")
	// ErrTooLarge signals the asset file exceeded the read cap; handler -> 413.
	ErrTooLarge = errors.New("assets: asset file exceeds size cap")
)
