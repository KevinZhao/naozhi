// Package cliinfo holds the CLI vocabulary other packages name without driving
// a CLI: process states, death reasons, the backend and model manifest rows,
// and the argv denylist. It imports only the standard library, so a config
// validator, a snapshot view or a wire-contract generator can use these
// without pulling internal/cli's process, shim and protocol machinery into
// its dependency closure. internal/cli re-exports each name.
package cliinfo
