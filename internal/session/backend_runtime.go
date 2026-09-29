package session

import "github.com/naozhi/naozhi/internal/session/backendstore"

// BackendRuntime is backendstore.Runtime: everything the router knows about
// one backend, as one row (G2 #2666).
type BackendRuntime = backendstore.Runtime
