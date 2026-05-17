package upstream

import "github.com/naozhi/naozhi/internal/session"

// Compile-time assertion that *session.Router satisfies the upstream
// consumer-side SessionRouter interface. Catches signature drift at
// build time so a Router method rename breaks `go test ./...` instead
// of silently rotting the structural-typing match. R222-CR-10.
var _ SessionRouter = (*session.Router)(nil)
