package schema

import (
	"encoding/binary"
	"errors"
	"io"
)

// IdxEntrySize is the fixed on-disk footprint of a single IdxEntry,
// in bytes. The idx sidecar is a stream of these packed little-endian.
//
// Layout (28 bytes):
//
//	offset 0  uint64  Seq         8
//	offset 8  int64   ByteOff     8
//	offset 16 int32   Len         4
//	offset 20 int64   TimeMS      8
const IdxEntrySize = 28

// IdxEntry is one row of the .idx sidecar. It maps a persisted record's
// Seq to its byte offset in the .log file plus its total framed length
// (including the length-prefix line AND the trailing newline).
//
// Having Len stored avoids an extra seek+read to determine how many
// bytes the record occupies when doing rotate tail-cut or recovery
// truncation — the startup recovery path (persist.Recover) uses
// ByteOff + Len as the idx-backed "safe edge" and truncates the log
// there if it exceeds log file size.
//
// The idx is sparse: only every N-th entry is written (default N=32,
// see persist.DefaultIdxStride). Readers use it as a coarse seek hint;
// the exact record at seq=S is found by starting from the nearest
// idx entry ≤ S and scanning forward.
type IdxEntry struct {
	Seq     uint64
	ByteOff int64
	Len     int32
	TimeMS  int64
}

// MarshalIdxEntry encodes e into out, which MUST be at least
// IdxEntrySize bytes long. The return value is `out[:IdxEntrySize]`
// for caller convenience.
func MarshalIdxEntry(out []byte, e IdxEntry) []byte {
	_ = out[IdxEntrySize-1] // bounds check hint
	binary.LittleEndian.PutUint64(out[0:8], e.Seq)
	binary.LittleEndian.PutUint64(out[8:16], uint64(e.ByteOff))
	binary.LittleEndian.PutUint32(out[16:20], uint32(e.Len))
	binary.LittleEndian.PutUint64(out[20:28], uint64(e.TimeMS))
	return out[:IdxEntrySize]
}

// UnmarshalIdxEntry decodes a single IdxEntry from buf. Returns
// ErrShortIdxBuf if buf is too short.
func UnmarshalIdxEntry(buf []byte) (IdxEntry, error) {
	if len(buf) < IdxEntrySize {
		return IdxEntry{}, ErrShortIdxBuf
	}
	return IdxEntry{
		Seq:     binary.LittleEndian.Uint64(buf[0:8]),
		ByteOff: int64(binary.LittleEndian.Uint64(buf[8:16])),
		Len:     int32(binary.LittleEndian.Uint32(buf[16:20])),
		TimeMS:  int64(binary.LittleEndian.Uint64(buf[20:28])),
	}, nil
}

// ReadIdxEntryAt reads a single idx entry from r at the given byte
// offset. Used by rotate to read the cut-point entry without slurping
// the whole idx file.
func ReadIdxEntryAt(r io.ReaderAt, offset int64) (IdxEntry, error) {
	var buf [IdxEntrySize]byte
	if _, err := r.ReadAt(buf[:], offset); err != nil {
		return IdxEntry{}, err
	}
	return UnmarshalIdxEntry(buf[:])
}

// AlignIdxSize rounds size down to the nearest IdxEntrySize multiple.
// Used by recovery to regularize a partially-written idx tail (the
// final write may have been interrupted mid-entry).
func AlignIdxSize(size int64) int64 {
	return (size / IdxEntrySize) * IdxEntrySize
}

// ErrShortIdxBuf is returned when an idx decode operation receives
// fewer than IdxEntrySize bytes.
var ErrShortIdxBuf = errors.New("schema: idx buffer shorter than IdxEntrySize")
