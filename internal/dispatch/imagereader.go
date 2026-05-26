package dispatch

import "os"

// ImageReader is the seam Dispatcher uses to resolve cli-extracted image
// paths (cli.ExtractImagePaths) to bytes for the outbound platform.Image
// payload. R245-ARCH-33 (#884): previously dispatch.go reached for
// os.ReadFile directly, leaving no way for tests to assert the read-
// success / read-failure branches without scribbling fixture files into
// /tmp.
//
// Production wires the default osImageReader{} (delegates to os.ReadFile)
// so callers do not need to set DispatcherConfig.ImageReader explicitly.
// Tests inject an in-memory map to drive the branches deterministically.
type ImageReader interface {
	// ReadFile mirrors os.ReadFile. Implementations must return a
	// non-nil error on read failure so the dispatch fallback (replace
	// path with "[图片]" sans attachment) keeps working.
	ReadFile(path string) ([]byte, error)
}

// osImageReader is the production ImageReader that delegates to
// os.ReadFile. NewDispatcher installs this when DispatcherConfig.
// ImageReader is nil so production wiring keeps zero-config.
type osImageReader struct{}

// ReadFile delegates to os.ReadFile.
func (osImageReader) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
