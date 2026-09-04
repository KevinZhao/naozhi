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
// registered here as `auth(s.<sub>H.HandleX)`. A `(s *Server) handle*`
// method is therefore forbidden except the static shell (handleDashboard).
//
// Enforced by tools/lint-server-handlers rule 1 (handle_decl: no Server
// handle* method outside exemptions.yaml handle_baseline) and rule 6
// (api_route_owner: no /api/* route registered on a Server method).
package server
