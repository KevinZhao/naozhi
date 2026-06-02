package cli

// R20260602190132-PERF-1 regression tests: shimMsgCode.UnmarshalJSON and the
// underlying parseJSONInt64Bytes helper must parse JSON integer tokens directly
// from []byte with no string(data) conversion (zero-alloc hot path).
//
// Table-driven cases cover:
//   - positive integers, negative integers (exit codes can be -1 for signal kill)
//   - zero (bare "0")
//   - empty input → error
//   - non-digit bytes → error
//   - leading '+' → error (JSON integers do not allow unary plus)
//   - leading zeros on multi-digit numbers → error
//   - values that would overflow int64 → error
//
// Present=true semantics are verified: UnmarshalJSON always sets Present=true
// on a successful parse, and the zero value (Present=false) must not change
// when no JSON key is present (the json package simply never calls UnmarshalJSON
// for absent keys — verified via json.Unmarshal round-trip below).

import (
	"encoding/json"
	"testing"
)

func TestParseJSONInt64Bytes(t *testing.T) {
	t.Parallel()

	type tc struct {
		input   string
		wantVal int64
		wantErr bool
	}

	cases := []tc{
		// Valid positives
		{input: "0", wantVal: 0},
		{input: "1", wantVal: 1},
		{input: "42", wantVal: 42},
		{input: "9223372036854775807", wantVal: 9223372036854775807}, // int64 max

		// Valid negatives (exit codes from signal kill, etc.)
		{input: "-1", wantVal: -1},
		{input: "-42", wantVal: -42},
		{input: "-9223372036854775808", wantVal: -9223372036854775808}, // int64 min

		// Invalid: empty
		{input: "", wantErr: true},

		// Invalid: bare minus with no digits
		{input: "-", wantErr: true},

		// Invalid: leading '+' (JSON disallows unary plus)
		{input: "+1", wantErr: true},
		{input: "+0", wantErr: true},

		// Invalid: non-digit characters
		{input: "abc", wantErr: true},
		{input: "1a2", wantErr: true},
		{input: "1.0", wantErr: true},
		{input: `"42"`, wantErr: true}, // JSON string, not integer
		{input: "true", wantErr: true},

		// Invalid: leading zeros on multi-digit numbers
		{input: "01", wantErr: true},
		{input: "007", wantErr: true},
		{input: "-01", wantErr: true},

		// Invalid: overflow beyond int64 range
		{input: "9223372036854775808", wantErr: true},  // int64 max + 1
		{input: "-9223372036854775809", wantErr: true}, // int64 min - 1
		{input: "99999999999999999999", wantErr: true}, // way over
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

// TestShimMsgCode_UnmarshalJSON_Present verifies that UnmarshalJSON sets
// Present=true and decodes the value correctly, including negative exit codes.
func TestShimMsgCode_UnmarshalJSON_Present(t *testing.T) {
	t.Parallel()

	cases := []struct {
		json    string
		wantVal int
	}{
		{`{"code": 0}`, 0},
		{`{"code": 1}`, 1},
		{`{"code": -1}`, -1},
		{`{"code": 127}`, 127},
		{`{"code": -127}`, -127},
	}

	for _, c := range cases {
		c := c
		t.Run(c.json, func(t *testing.T) {
			t.Parallel()
			var msg shimMsg
			if err := json.Unmarshal([]byte(c.json), &msg); err != nil {
				t.Fatalf("json.Unmarshal(%q) error: %v", c.json, err)
			}
			if !msg.Code.Present {
				t.Errorf("Code.Present = false, want true for %q", c.json)
			}
			if msg.Code.Value != c.wantVal {
				t.Errorf("Code.Value = %d, want %d for %q", msg.Code.Value, c.wantVal, c.json)
			}
		})
	}
}

// TestShimMsgCode_AbsentKey_PresentFalse verifies that an absent "code" key
// leaves Present=false (the zero value), i.e. UnmarshalJSON is never called
// for absent keys.
func TestShimMsgCode_AbsentKey_PresentFalse(t *testing.T) {
	t.Parallel()
	var msg shimMsg
	if err := json.Unmarshal([]byte(`{"type":"cli_exited"}`), &msg); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}
	if msg.Code.Present {
		t.Errorf("Code.Present = true for absent key, want false")
	}
	if msg.Code.Value != 0 {
		t.Errorf("Code.Value = %d for absent key, want 0", msg.Code.Value)
	}
}

// TestShimMsgCode_InvalidToken_Error verifies that a non-integer JSON value
// for "code" propagates an error through json.Unmarshal.
func TestShimMsgCode_InvalidToken_Error(t *testing.T) {
	t.Parallel()
	var msg shimMsg
	err := json.Unmarshal([]byte(`{"code":"not-an-int"}`), &msg)
	if err == nil {
		t.Errorf("expected error for non-integer code token, got nil")
	}
}
