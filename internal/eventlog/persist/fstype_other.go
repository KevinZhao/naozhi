//go:build !linux && !darwin

package persist

import "errors"

// DetectFS on platforms other than Linux/macOS returns "unknown" without a
// syscall so experimental Windows / BSD builds do not crash.
func DetectFS(dir string) FSDetection {
	return FSDetection{
		Type:      FSTypeUnknown,
		Supported: false,
		Err:       errors.New("persist: filesystem detection not implemented on this platform"),
	}
}
