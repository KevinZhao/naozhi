package ccassets

import (
	"io"

	"github.com/naozhi/naozhi/internal/assets"
)

// maxRawBytes caps a single raw-asset read; far beyond any real SKILL.md.
const maxRawBytes = 1 << 20

// readCapped reads up to cap bytes from path, returning ErrTooLarge rather
// than a truncated body. The path must already be validated by resolveUnder.
//
// openNoFollow closes the TOCTOU window between resolveUnder's EvalSymlinks
// and this open (a writer to ~/.claude could swap the final component for a
// symlink). Unix uses O_NOFOLLOW; windows falls back to Lstat→Open.
func readCapped(path string, cap int64) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Read cap+1 to distinguish "exactly cap" from "over cap".
	data, err := io.ReadAll(io.LimitReader(f, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > cap {
		return nil, assets.ErrTooLarge
	}
	return data, nil
}
