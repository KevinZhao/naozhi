package persist

import (
	"runtime"
	"testing"
)

// TestDetectFS_TempDir_Supported sanity-checks the happy path on
// Linux CI: /tmp is ext4 or tmpfs depending on the runner. We just
// assert that detection returns a non-empty Type string without an
// err.
func TestDetectFS_TempDir_Supported(t *testing.T) {
	det := DetectFS(t.TempDir())
	if det.Err != nil {
		t.Fatalf("DetectFS on tmpdir erred: %v", det.Err)
	}
	if det.Type == "" {
		t.Errorf("DetectFS returned empty Type on a valid directory")
	}
	// On Linux most runners are tmpfs or ext4; on macOS it's apfs.
	// Either way Type should be one of our known labels.
	switch det.Type {
	case FSTypeExt4, FSTypeXFS, FSTypeBtrfs, FSTypeAPFS,
		FSTypeTmpfs, FSTypeOverlay, FSTypeFUSE, FSTypeUnknown:
		// ok
	default:
		t.Errorf("unknown FS label: %q", det.Type)
	}
}

// TestDetectFS_MissingDir fails cleanly with an Err set and
// Supported=false. A /health probe must never crash because the
// events directory is temporarily unreachable.
func TestDetectFS_MissingDir(t *testing.T) {
	det := DetectFS("/nonexistent/path/that/will/never/exist/12345")
	if det.Type != FSTypeUnknown {
		t.Errorf("missing dir Type=%q, want unknown", det.Type)
	}
	if det.Supported {
		t.Errorf("missing dir reported supported=true")
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		if det.Err == nil {
			t.Errorf("missing dir Err=nil (expected ENOENT)")
		}
	}
}

// TestFSTypeConstants locks the public label strings. A change here
// is an API break — /health readers and dashboards key off these
// exact strings.
func TestFSTypeConstants(t *testing.T) {
	want := map[string]string{
		"ext4":      FSTypeExt4,
		"xfs":       FSTypeXFS,
		"apfs":      FSTypeAPFS,
		"tmpfs":     FSTypeTmpfs,
		"nfs":       FSTypeNFS,
		"overlayfs": FSTypeOverlay,
		"btrfs":     FSTypeBtrfs,
		"fuse":      FSTypeFUSE,
		"unknown":   FSTypeUnknown,
	}
	for literal, got := range want {
		if got != literal {
			t.Errorf("FSType constant drifted: %q != literal %q", got, literal)
		}
	}
}

// TestPersister_ExposesFSDetection ensures the cached detection is
// surfaced through Stats() so the /health wiring works.
func TestPersister_ExposesFSDetection(t *testing.T) {
	p, _ := newTestPersister(t)
	s := p.Stats()
	if s.FSType == "" {
		t.Errorf("Stats.FSType empty")
	}
	// Whatever we're on, FSSupported mirrors the FS detection.
	if s.FSSupported != p.FS().Supported {
		t.Errorf("Stats.FSSupported=%v, FS().Supported=%v",
			s.FSSupported, p.FS().Supported)
	}
}

// TestPersister_FSMethodOnNil returns a safe zero without panicking.
func TestPersister_FSMethodOnNil(t *testing.T) {
	var p *Persister
	fs := p.FS()
	if fs.Type != FSTypeUnknown {
		t.Errorf("nil receiver Type=%q, want unknown", fs.Type)
	}
}
