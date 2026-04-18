//go:build !linux

package osutil

// SdNotify is a no-op on non-Linux platforms (macOS, etc.).
func SdNotify(_ string) error {
	return nil
}
