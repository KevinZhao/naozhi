package ccassets

import (
	"io"
	"os"

	"github.com/naozhi/naozhi/internal/assets"
)

// maxRawBytes caps a single raw-asset read (RFC §5 / evaluation K). 1 MiB is
// far beyond any real SKILL.md / agent.md / hooks.json.
const maxRawBytes = 1 << 20

// readCapped reads up to cap bytes from path. If the file is larger than cap
// it returns errTooLarge rather than a truncated body (a truncated SKILL.md
// would render misleadingly). The path must already be validated by the
// caller (resolveUnder).
func readCapped(path string, cap int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Read cap+1 so we can distinguish "exactly cap" from "over cap".
	data, err := io.ReadAll(io.LimitReader(f, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > cap {
		return nil, assets.ErrTooLarge
	}
	return data, nil
}
