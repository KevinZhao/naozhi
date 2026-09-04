package discovery

import "errors"

// ErrUnsupportedPlatform is returned by platform-stubbed proc helpers on
// systems where the underlying syscall surface is not implemented. Declared
// without a build tag so errors.Is works across all compilation targets.
var ErrUnsupportedPlatform = errors.New("operation not supported on this platform")
