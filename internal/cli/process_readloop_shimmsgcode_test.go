package cli

// parseJSONInt64Bytes backs shimMsgCode.UnmarshalJSON, decoding the
// cli_exited `code` token straight from []byte (R20260602190132-PERF-1).
// Present/absent semantics are covered by shim_msg_contract_test.go.

import "testing"

func TestParseJSONInt64Bytes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input   string
		wantVal int64
		wantErr bool
	}{
		{input: "0", wantVal: 0},
		{input: "-1", wantVal: -1}, // signal-kill exit code
		{input: "9223372036854775807", wantVal: 9223372036854775807},
		{input: "9223372036854775808", wantErr: true}, // int64 max + 1
		{input: "01", wantErr: true},                  // leading zero
		{input: "1.0", wantErr: true},
		{input: `"42"`, wantErr: true}, // JSON string, not integer
		{input: "", wantErr: true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.input, func(t *testing.T) {
			t.Parallel()
			got, err := parseJSONInt64Bytes([]byte(c.input))
			if c.wantErr {
				if err == nil {
					t.Errorf("parseJSONInt64Bytes(%q) = %d, nil; want error", c.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parseJSONInt64Bytes(%q) unexpected error: %v", c.input, err)
				return
			}
			if got != c.wantVal {
				t.Errorf("parseJSONInt64Bytes(%q) = %d, want %d", c.input, got, c.wantVal)
			}
		})
	}
}

// TestShimMsgCode_LargeValue_NoTruncation locks R202606f-GO-001: Value is
// int64 so an exit code beyond the 32-bit int range round-trips without
// silently wrapping on a 32-bit build (where `int` is 32-bit and the prior
// `int(v)` narrowing truncated).
func TestShimMsgCode_LargeValue_NoTruncation(t *testing.T) {
	t.Parallel()
	const big = int64(1) << 40 // 1099511627776, far past 2^31-1
	var c shimMsgCode
	if err := c.UnmarshalJSON([]byte("1099511627776")); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if c.Value != big {
		t.Errorf("Value = %d, want %d (truncated to 32-bit?)", c.Value, big)
	}
}
