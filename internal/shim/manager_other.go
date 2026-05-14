//go:build !linux && !darwin

package shim

import (
	"context"
	"errors"
)

// moveToShimsCgroup is a no-op on platforms where shim lifecycle separation
// is not implemented. Production naozhi only ships on Linux (cgroup/systemd
// scope) and Darwin (launchd auto-reparenting); other GOOS builds compile so
// `go build ./...` succeeds in CI but the shim cannot survive a naozhi
// restart on those targets.
func moveToShimsCgroup(_ context.Context, _, _ int) {}

// shimPIDBinaryMismatch returns an error on unsupported platforms; callers
// treat err != nil as "skip the gate", matching the Linux readlink-failure
// branch. The functional consequence is that PID-reuse detection is
// disabled on these builds, but reconnect still proceeds based on the
// auth-token check carried in the state file.
func shimPIDBinaryMismatch(_ int, _ string) (bool, error) {
	return false, errors.New("binary identity check not implemented on this platform")
}
