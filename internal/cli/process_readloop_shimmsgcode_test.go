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
