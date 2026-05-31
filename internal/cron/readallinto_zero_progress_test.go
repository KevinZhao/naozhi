package cron

import (
	"errors"
	"io"
	"testing"
)

// zeroProgressReader is an io.Reader that returns (0, nil) indefinitely,
// simulating a FUSE or pathological io.Reader implementation that is
// contractually allowed to make no forward progress without returning an
// error.
type zeroProgressReader struct{}

func (zeroProgressReader) Read(_ []byte) (int, error) { return 0, nil }

// eofAfterNReader returns (0, nil) for the first stallCount calls, then
// returns (0, io.EOF) to verify that normal EOF paths still work after
// zero-progress reads below the guard threshold.
type eofAfterNReader struct {
	stalls int
	total  int
}

func (r *eofAfterNReader) Read(_ []byte) (int, error) {
	if r.total < r.stalls {
		r.total++
		return 0, nil
	}
	return 0, io.EOF
}

// TestReadAllIntoReader_ZeroProgress_R171023_CR_007 verifies that
// readAllIntoReader does not hang when the reader repeatedly returns (0, nil).
// The guard must fire within zeroProgressLimit iterations and return
// io.ErrNoProgress.
func TestReadAllIntoReader_ZeroProgress_R171023_CR_007(t *testing.T) {
	t.Parallel()

	_, err := readAllIntoReader(zeroProgressReader{}, nil)
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("readAllIntoReader with zero-progress reader: got err=%v, want io.ErrNoProgress", err)
	}
}

// TestReadAllIntoReader_BelowGuard_EOF_R171023_CR_007 verifies that a single
// (0, nil) followed by (0, io.EOF) still succeeds — i.e. the guard only fires
// at the threshold, not on the first zero read.
func TestReadAllIntoReader_BelowGuard_EOF_R171023_CR_007(t *testing.T) {
	t.Parallel()

	// One stall is below zeroProgressLimit (2), so EOF must succeed.
	r := &eofAfterNReader{stalls: 1}
	data, err := readAllIntoReader(r, nil)
	if err != nil {
		t.Fatalf("readAllIntoReader with 1 stall then EOF: got err=%v, want nil", err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty data, got %d bytes", len(data))
	}
}
