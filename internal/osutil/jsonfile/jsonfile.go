// Package jsonfile loads a JSON snapshot file the way a state store needs it:
// bounded, symlink-refusing, and never silently destroying a file it could not
// parse. J4 of #2548.
//
// Five stores read a JSON snapshot of naozhi's own state and each arrived at a
// different water level:
//
//	internal/cron/store.go             O_NOFOLLOW + Fstat + cap + .corrupt rename
//	internal/session/store.go          cap + .corrupt rename
//	internal/uiprefs/store.go          cap
//	internal/discovery/retired_store.go  (none; has its own version field)
//	internal/session/runhistory/store.go (none)
//
// The interesting difference is not the missing caps — it is what a caller is
// allowed to conclude from a failure. Reading a snapshot has three outcomes, and
// only two of them are safe to start empty on:
//
//	parsed          → use the value
//	no file, or the file was moved aside → starting empty destroys nothing
//	could not read  → THE FILE IS STILL THERE; starting empty means the next
//	                  atomic save overwrites the operator's real state (#469)
//
// cron is the only store that got this right. runhistory conflated all three:
// readRunFile's error was answered with `continue`, so a corrupt run file was
// skipped and left on disk forever — never parsed, and never reached the
// retention GC below it either, because the skip happened first.
//
// Load returns that outcome explicitly, so the unsafe case is the one a caller
// cannot ignore, and a caller that wants to report a corrupt file separately from
// a missing one can (internal/discovery/retired_store.go does: its parse error is
// a diagnostic channel, not a construction failure).
package jsonfile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

// ErrSymlink is returned when the path's final component is a symlink. Callers
// log this distinctly from an I/O error: a symlink appearing where a state file
// belongs is an attempted swap, not a disk problem.
var ErrSymlink = errors.New("jsonfile: refused to follow symlink")

// Outcome says what Load found. The zero value is Absent, so a caller that
// forgets to check never treats an unread file as parsed data.
type Outcome int

const (
	// Absent: no file, or an empty one (a half-finished write has nothing in it
	// to preserve). Starting from empty state destroys nothing.
	Absent Outcome = iota
	// Parsed: the returned value is usable. The only outcome that says so.
	Parsed
	// CorruptPreserved: the file failed to parse and was renamed to
	// <path>.corrupt.<ts>.<nonce>. Starting empty destroys no evidence, but the
	// caller may want to report it.
	CorruptPreserved
	// CorruptLeft: the file failed to parse and was left in place per
	// LeaveCorrupt. The next atomic save will overwrite it.
	CorruptLeft
)

// CorruptPolicy decides what happens to a file that failed to parse.
type CorruptPolicy int

const (
	// PreserveCorrupt renames the unparseable file to <path>.corrupt.<ts>.<nonce>
	// so the next atomic save cannot overwrite the evidence of a partial write
	// (#673). This is the zero value: a store whose contents cannot be
	// reconstructed from anywhere else wants it, and that is most of them.
	PreserveCorrupt CorruptPolicy = iota
	// LeaveCorrupt leaves the file where it is. Only for stores carrying nothing
	// irreplaceable, where a .corrupt sibling per bad parse is litter rather than
	// evidence — uiprefs is the one, and it made that call before this package
	// existed.
	LeaveCorrupt
)

// Options configures one Load. MaxBytes and Label are both required; a zero
// MaxBytes is a programming error rather than "unbounded", because unbounded is
// the thing this package exists to prevent.
type Options struct {
	// MaxBytes caps the file. Over the cap Load returns an error and leaves the
	// file in place: an oversized state file is usually real operator data, and
	// moving it aside would hide it.
	MaxBytes int64
	// Label names the file in log lines and errors, e.g. "cron store".
	Label string
	// Corrupt decides whether an unparseable file is moved aside. Zero value
	// preserves it.
	Corrupt CorruptPolicy
}

// Load reads path and JSON-decodes it into T. A non-nil error means the original
// file is still on disk — size cap, I/O error, not a regular file, a symlink, or
// the corrupt-rename itself failed. Callers MUST NOT continue with empty state
// in that case: the next atomic save would clobber the real file. Otherwise the
// Outcome says what happened, and only Parsed means the value is usable.
//
// The absolute path is kept out of the returned error (it may reach an HTTP
// response) and logged instead.
func Load[T any](path string, opts Options) (T, Outcome, error) {
	var zero T
	if opts.MaxBytes <= 0 {
		return zero, Absent, fmt.Errorf("jsonfile: MaxBytes must be > 0 for %s", opts.Label)
	}
	if path == "" {
		return zero, Absent, nil
	}
	// OpenFile(O_NOFOLLOW|O_CLOEXEC) refuses a symlinked path atomically — no
	// Lstat→Open TOCTOU window (#829). Fstat below validates the inode actually
	// opened is a regular file, so a fifo/socket/device never reaches Unmarshal.
	f, err := openNoFollow(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return zero, Absent, nil
		}
		if isSymlinkErr(err) {
			slog.Warn(opts.Label+": path is a symlink; refusing to follow", "path", path)
			return zero, Absent, fmt.Errorf("%s: %w", opts.Label, ErrSymlink)
		}
		slog.Warn("open "+opts.Label+" failed", "path", path, "err", err)
		return zero, Absent, fmt.Errorf("open %s: %w", opts.Label, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		slog.Warn(opts.Label+": fstat failed; refusing to load", "path", path, "err", err)
		return zero, Absent, fmt.Errorf("fstat %s: %w", opts.Label, err)
	}
	if !fi.Mode().IsRegular() {
		slog.Warn(opts.Label+": not a regular file; refusing to load",
			"path", path, "mode", fi.Mode().String())
		return zero, Absent, fmt.Errorf("%s: not a regular file, refusing to load", opts.Label)
	}
	data, err := io.ReadAll(io.LimitReader(f, opts.MaxBytes+1))
	if err != nil {
		slog.Warn("read "+opts.Label+" failed", "path", path, "err", err)
		return zero, Absent, fmt.Errorf("read %s: %w", opts.Label, err)
	}
	if int64(len(data)) > opts.MaxBytes {
		// LimitReader stopped at cap+1, so only "at least" is known.
		slog.Warn(opts.Label+" exceeds size cap",
			"path", path, "size", len(data), "cap", opts.MaxBytes)
		return zero, Absent, fmt.Errorf("%s exceeds size cap (at least %d bytes, cap=%d bytes); refusing to load — inspect the file or move it aside before restarting",
			opts.Label, len(data), opts.MaxBytes)
	}
	// An empty file is a half-finished write, not valid JSON. Treat it as absent:
	// there is nothing in it to preserve.
	if len(data) == 0 {
		return zero, Absent, nil
	}

	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		if opts.Corrupt == LeaveCorrupt {
			slog.Warn("parse "+opts.Label+" failed; file left in place", "err", err, "path", path)
			return zero, CorruptLeft, nil
		}
		// Move the unparseable file aside so the next atomic save does not
		// overwrite the evidence. If the rename fails, return an error — saving
		// empty state over the original would destroy it.
		corruptPath := path + ".corrupt." + time.Now().UTC().Format("20060102-150405") + "." + randomNonce()
		if renameErr := os.Rename(path, corruptPath); renameErr != nil {
			return zero, Absent, fmt.Errorf("parse %s failed (%v); could not rename: %w",
				opts.Label, err, renameErr)
		}
		slog.Warn("parse "+opts.Label+" failed; corrupt file preserved",
			"err", err, "path", path, "corrupt_path", corruptPath)
		return zero, CorruptPreserved, nil
	}
	return v, Parsed, nil
}

// randomNonce keeps two instances sharing one data dir from colliding on the
// same corrupt path within the same second. A time-derived fallback covers the
// never-expected case of crypto/rand failing.
func randomNonce() string {
	var rb [4]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
	}
	return hex.EncodeToString(rb[:])
}
