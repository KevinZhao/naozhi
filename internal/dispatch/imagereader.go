package dispatch

import "os"

// ImageReader is the seam Dispatcher uses to resolve cli-extracted image
// paths to bytes for the outbound platform.Image payload, so tests can drive
// the read-success / read-failure branches without fixture files (#884).
// Production defaults to osImageReader{}.
type ImageReader interface {
	// ReadFile mirrors os.ReadFile. Implementations must return a
	// non-nil error on read failure so the dispatch fallback (replace
	// path with "[图片]" sans attachment) keeps working.
	ReadFile(path string) ([]byte, error)
}

// osImageReader is the production ImageReader; installed when
// DispatcherConfig.ImageReader is nil.
type osImageReader struct{}

func (osImageReader) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
