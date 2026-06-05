package upstream

import "github.com/naozhi/naozhi/internal/session"

// Compile-time assertion that *session.Router satisfies the upstream
// consumer-side SessionRouter interface. Catches signature drift at
// build time so a Router method rename breaks `go test ./...` instead
// of silently rotting the structural-typing match. R222-CR-10.
var _ SessionRouter = (*session.Router)(nil)

// Individually pin the three forward-compatible sub-interfaces
// (consumer.go SessionLookup/SessionLifecycle/SessionMutator) against
// *session.Router. The union pin above only catches drift transitively;
// asserting each sub-interface directly means a signature change that
// breaks just one narrow capability surfaces against that sub-interface,
// not merely via the composed union — matching the documented contract
// that consumers may depend on the narrowest sub-interface they need.
// Refs #580 / RFC docs/rfc/router-god-object-split.md §8.2 P0 pin-
// hardening; this is NOT a convergence to a central union (§7.2 forbids).
var _ SessionLookup = (*session.Router)(nil)
var _ SessionLifecycle = (*session.Router)(nil)
var _ SessionMutator = (*session.Router)(nil)
