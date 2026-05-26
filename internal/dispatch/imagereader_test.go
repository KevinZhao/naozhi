package dispatch

import (
	"errors"
	"testing"
)

// TestImageReader_DefaultIsOsImageReader pins that NewDispatcher installs
// osImageReader{} when DispatcherConfig.ImageReader is left nil, so
// existing production wiring keeps reading attachment paths off disk
// without an explicit field. R245-ARCH-33 (#884).
func TestImageReader_DefaultIsOsImageReader(t *testing.T) {
	d := NewDispatcher(DispatcherConfig{
		AllowMissingSender: true,
	})
	if d.imageReader == nil {
		t.Fatalf("NewDispatcher left d.imageReader nil; default osImageReader{} must be installed")
	}
	if _, ok := d.imageReader.(osImageReader); !ok {
		t.Errorf("default ImageReader = %T, want dispatch.osImageReader", d.imageReader)
	}
}

// TestImageReader_OverrideHonoured pins that DispatcherConfig.ImageReader,
// when non-nil, replaces the default — the seam tests need to drive the
// read-success / read-failure branches without writing to /tmp.
func TestImageReader_OverrideHonoured(t *testing.T) {
	fake := &fakeImageReader{}
	d := NewDispatcher(DispatcherConfig{
		AllowMissingSender: true,
		ImageReader:        fake,
	})
	if d.imageReader != ImageReader(fake) {
		t.Fatalf("DispatcherConfig.ImageReader override ignored; got %T", d.imageReader)
	}
}

// TestOsImageReader_ReadFileMissingReturnsError pins the sole production
// guarantee callers in sendAndReply rely on: a missing file produces a
// non-nil error so the path is rewritten to "[图片]" without a partial
// attachment. We do not assert the exact error type — that is os.ReadFile's
// contract — only that one is returned.
func TestOsImageReader_ReadFileMissingReturnsError(t *testing.T) {
	r := osImageReader{}
	_, err := r.ReadFile("/nonexistent/dispatch-imagereader-test.png")
	if err == nil {
		t.Fatalf("osImageReader.ReadFile on missing path returned nil error")
	}
}

// fakeImageReader records calls and returns canned results. Useful for
// tests asserting that sendAndReply routes attachment reads through the
// seam rather than the global os.ReadFile.
type fakeImageReader struct {
	files map[string][]byte
	err   error
}

func (f *fakeImageReader) ReadFile(path string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.files == nil {
		return nil, errors.New("fakeImageReader: no files configured")
	}
	data, ok := f.files[path]
	if !ok {
		return nil, errors.New("fakeImageReader: not found")
	}
	return data, nil
}
