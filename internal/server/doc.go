// Package server owns only the HTTP pipe of naozhi:
//
//   - mux construction and route wiring (routes.go)
//   - auth / rate-limit / body-cap middleware
//   - the WebSocket hub and its upgrade handlers (/ws, /ws-node)
//   - liveness and health endpoints (/health, /livez, /readyz)
//   - the static dashboard shell and its assets (/dashboard, /static/*, sw.js)
//   - platform webhook registration (Platform.RegisterRoutes)
//   - the send/upload pipeline (SendHandler; leaves with Phase 3f/4c)
//
// Every other /api/* route handler lives in an internal/dashboard/<sub>
// package, receives its collaborators through a Deps struct, and is
// registered here as `auth(s.<sub>H.HandleX)`. An HTTP handler declared in
// this package is therefore one of the pipe pieces above.
//
// Enforced by tools/lint-server-handlers rule 1 (handle_decl: no HTTP handler
// declaration in this package, on any receiver or none, outside
// exemptions.yaml handle_baseline).
package server
