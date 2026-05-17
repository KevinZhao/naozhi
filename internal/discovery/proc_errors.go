package discovery

import "errors"

// ErrUnsupportedPlatform is returned by platform-stubbed proc helpers
// on systems where the underlying syscall surface is not implemented.
// Declared here (no build tag) so all platform builds reference the
// same error value, ensuring errors.Is comparisons work across the
// linux / darwin / windows compilation targets.
var ErrUnsupportedPlatform = errors.New("operation not supported on this platform")
