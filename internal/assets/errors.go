package assets

import "errors"

// Sentinel errors from Provider.ReadRaw; a leaf so provider and handler can errors.Is them.
var (
	// ErrNotFound covers both "no such asset" and "path escaped the allowed
	// root", so the API does not leak whether a traversal target exists (404).
	ErrNotFound = errors.New("assets: not found")
	// ErrTooLarge signals the asset file exceeded the read cap (413).
	ErrTooLarge = errors.New("assets: asset file exceeds size cap")
)
