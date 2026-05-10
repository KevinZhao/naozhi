//go:build !linux && !darwin

package persist

import "errors"

// DetectFS on unsupported platforms returns "unknown" without
// attempting a syscall. The production target is Linux (with macOS
// as a best-effort dev surface); Windows / BSD builds shouldn't
// crash if someone experiments.
func DetectFS(dir string) FSDetection {
	return FSDetection{
		Type:      FSTypeUnknown,
		Supported: false,
		Err:       errors.New("persist: filesystem detection not implemented on this platform"),
	}
}
