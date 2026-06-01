package persist

import (
	"bufio"
	"errors"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// TestReadRecord_LengthDigitBoundary pins the exact maxLengthDigits gate so
// an off-by-one drift in the constant (or the `len(digits) > maxLengthDigits`
// comparison) is caught. The existing TestReadRecord_MalformedLength only
// covers the over-cap case (12 digits) and never exercises the boundary:
//
//   - maxLengthDigits digits: ACCEPTED by the digit-count gate, then
//     rejected by the MaxRecordBytes cap (an 11-digit value is always
//     >> 4 MiB). The distinguishing signal is ErrRecordTooLarge, NOT
//     ErrMalformedFrame — proving the bytes passed the digit gate.
//   - maxLengthDigits+1 digits: rejected by the digit-count gate as
//     ErrMalformedFrame before any numeric parse.
//
// If maxLengthDigits were lowered to 10, the first case would wrongly
// surface ErrMalformedFrame; if raised to 12, the second case would fall
// through to the numeric parse. Either drift fails here.
func TestReadRecord_LengthDigitBoundary(t *testing.T) {
	atCap := strings.Repeat("9", maxLengthDigits) // exactly maxLengthDigits digits
	overCap := strings.Repeat("9", maxLengthDigits+1)

	t.Run("at_cap_passes_digit_gate", func(t *testing.T) {
		data := atCap + "\n{}\n"
		_, err := ReadRecord(bufio.NewReader(strings.NewReader(data)))
		if !errors.Is(err, schema.ErrRecordTooLarge) {
			t.Fatalf("len=%q: err=%v, want ErrRecordTooLarge (digit gate must accept %d digits)",
				atCap, err, maxLengthDigits)
		}
		if errors.Is(err, ErrMalformedFrame) {
			t.Errorf("len=%q wrongly rejected by digit-count gate as malformed", atCap)
		}
	})

	t.Run("over_cap_rejected_as_malformed", func(t *testing.T) {
		data := overCap + "\n{}\n"
		_, err := ReadRecord(bufio.NewReader(strings.NewReader(data)))
		if !errors.Is(err, ErrMalformedFrame) {
			t.Fatalf("len=%q (%d digits): err=%v, want ErrMalformedFrame",
				overCap, maxLengthDigits+1, err)
		}
	})
}
