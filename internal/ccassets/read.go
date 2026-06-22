package ccassets

import (
	"io"

	"github.com/naozhi/naozhi/internal/assets"
)

// maxRawBytes caps a single raw-asset read (RFC §5 / evaluation K). 1 MiB is
// far beyond any real SKILL.md / agent.md / hooks.json.
const maxRawBytes = 1 << 20

// readCapped reads up to cap bytes from path. If the file is larger than cap
// it returns errTooLarge rather than a truncated body (a truncated SKILL.md
// would render misleadingly). The path must already be validated by the
// caller (resolveUnder).
//
// O_NOFOLLOW closes the TOCTOU window between resolveUnder's EvalSymlinks
// check and this open: an attacker with write access to ~/.claude could swap
// the final path component for a symlink to an arbitrary file after the check
// but before the read. With O_NOFOLLOW the open fails if the final component
// is a symlink, mirroring the O_NOFOLLOW hardening in dashboard files.go
// (R219-SEC-2) and memory handler.go (R20260606-SEC-6). [R202606d-SEC-1]
//
// openNoFollow is platform-specialised: the unix build uses O_NOFOLLOW for a
// kernel-atomic refusal; the windows build (which lacks O_NOFOLLOW) falls back
// to an Lstat→Open shim. naozhi's production target is Linux.
func readCapped(path string, cap int64) ([]byte, error) {
	f, err := openNoFollow(path)
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
