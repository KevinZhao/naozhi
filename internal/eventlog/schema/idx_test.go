package schema

import (
	"bytes"
	"errors"
	"testing"
)

// TestIdxEntry_MarshalUnmarshalRoundTrip locks the 28-byte on-disk layout
// so a future "improve by packing smaller" change shows up as a compile
// failure in this test rather than as silent corruption of existing
// idx files.
func TestIdxEntry_MarshalUnmarshalRoundTrip(t *testing.T) {
	cases := []IdxEntry{
		{Seq: 0, ByteOff: 0, Len: 42, TimeMS: 0},
		{Seq: 1, ByteOff: 42, Len: 318, TimeMS: 1700000001000},
		// Extreme values — int64 upper half.
		{Seq: 1<<63 - 1, ByteOff: 1<<62 - 1, Len: 1<<31 - 1, TimeMS: 1<<62 - 1},
	}
	var buf [IdxEntrySize]byte
	for _, want := range cases {
		MarshalIdxEntry(buf[:], want)
		got, err := UnmarshalIdxEntry(buf[:])
		if err != nil {
			t.Fatalf("unmarshal %+v: %v", want, err)
		}
		if got != want {
			t.Errorf("round-trip: got %+v, want %+v", got, want)
		}
	}
}

// TestIdxEntry_SizeConstant locks the wire width. Bumping IdxEntrySize is
// a storage-format change; every existing <keyhash>.idx becomes
// unreadable on upgrade. This test must fail on any drift so the change
// forces a conscious migration plan.
func TestIdxEntry_SizeConstant(t *testing.T) {
	if IdxEntrySize != 28 {
		t.Fatalf("IdxEntrySize=%d, want 28 (see RFC §3.1.3)", IdxEntrySize)
	}
}

// TestIdxEntry_LittleEndianLayout pins the byte ordering. If someone ever
// swaps to big-endian or re-orders fields the hex pattern changes and
// this test catches it before users get corrupted files.
func TestIdxEntry_LittleEndianLayout(t *testing.T) {
	e := IdxEntry{
		Seq:     0x0102030405060708,
		ByteOff: 0x1112131415161718,
		Len:     0x21222324,
		TimeMS:  0x3132333435363738,
	}
	var buf [IdxEntrySize]byte
	MarshalIdxEntry(buf[:], e)
	want := []byte{
		0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, // Seq (LE)
		0x18, 0x17, 0x16, 0x15, 0x14, 0x13, 0x12, 0x11, // ByteOff (LE)
		0x24, 0x23, 0x22, 0x21, //                        Len (LE)
		0x38, 0x37, 0x36, 0x35, 0x34, 0x33, 0x32, 0x31, // TimeMS (LE)
	}
	if !bytes.Equal(buf[:], want) {
		t.Errorf("layout drift\n got:  % x\n want: % x", buf[:], want)
	}
}

// TestUnmarshalIdxEntry_ShortBuf confirms the short-buf error path — the
// recovery code relies on this to detect a truncated-mid-entry idx tail.
func TestUnmarshalIdxEntry_ShortBuf(t *testing.T) {
	_, err := UnmarshalIdxEntry(make([]byte, IdxEntrySize-1))
	if !errors.Is(err, ErrShortIdxBuf) {
		t.Errorf("short buf err = %v, want ErrShortIdxBuf", err)
	}
}

// TestAlignIdxSize covers the startup recovery helper. Any file size
// that is NOT a multiple of IdxEntrySize indicates a torn write at the
// tail; we round down so the reader never pulls a half-entry.
func TestAlignIdxSize(t *testing.T) {
	tests := []struct {
		in, want int64
	}{
		{0, 0},
		{IdxEntrySize, IdxEntrySize},
		{IdxEntrySize * 3, IdxEntrySize * 3},
		{IdxEntrySize*3 + 1, IdxEntrySize * 3},  // partial tail dropped
		{IdxEntrySize*3 + 27, IdxEntrySize * 3}, // partial tail dropped
		{IdxEntrySize - 1, 0},                   // single torn entry at head → empty idx
	}
	for _, tc := range tests {
		if got := AlignIdxSize(tc.in); got != tc.want {
			t.Errorf("AlignIdxSize(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestReadIdxEntryAt exercises the io.ReaderAt path the rotate logic
// uses — it reads the cut-point entry without slurping the whole idx.
func TestReadIdxEntryAt(t *testing.T) {
	var data [IdxEntrySize * 3]byte
	want := IdxEntry{Seq: 7, ByteOff: 4096, Len: 128, TimeMS: 1700000000000}
	MarshalIdxEntry(data[IdxEntrySize*2:], want)

	got, err := ReadIdxEntryAt(bytes.NewReader(data[:]), IdxEntrySize*2)
	if err != nil {
		t.Fatalf("ReadIdxEntryAt: %v", err)
	}
	if got != want {
		t.Errorf("ReadIdxEntryAt: got %+v, want %+v", got, want)
	}
}
