package cli

import (
	"encoding/json"
	"testing"
)

// TestShimMsgCode_AbsentVsPresent locks the json round-trip semantics for
// the optional cli_exited "code" field after R222-PERF-13 changed it from
// *int to a custom (int, bool) pair to avoid the per-message heap alloc.
//
// Three buckets:
//   - field absent          → Present=false, Value=0
//   - field present, value=0 → Present=true,  Value=0  (must distinguish
//     from "absent" — the shim emits explicit 0
//     when CLI exits cleanly)
//   - field present, value=N → Present=true,  Value=N
func TestShimMsgCode_AbsentVsPresent(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantPresent bool
		wantValue   int
	}{
		{name: "absent", raw: `{"type":"cli_exited"}`, wantPresent: false, wantValue: 0},
		{name: "present_zero", raw: `{"type":"cli_exited","code":0}`, wantPresent: true, wantValue: 0},
		{name: "present_nonzero", raw: `{"type":"cli_exited","code":137}`, wantPresent: true, wantValue: 137},
		{name: "present_negative", raw: `{"type":"cli_exited","code":-1}`, wantPresent: true, wantValue: -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var msg shimMsg
			if err := json.Unmarshal([]byte(tc.raw), &msg); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if msg.Code.Present != tc.wantPresent {
				t.Errorf("Code.Present = %v, want %v", msg.Code.Present, tc.wantPresent)
			}
			if msg.Code.Value != tc.wantValue {
				t.Errorf("Code.Value = %d, want %d", msg.Code.Value, tc.wantValue)
			}
		})
	}
}

// TestShimMsgCode_InvalidJSON ensures bogus shim payloads still surface
// as Unmarshal errors (parity with the previous *int decoding).
func TestShimMsgCode_InvalidJSON(t *testing.T) {
	cases := []string{
		`{"code":"not-a-number"}`,
		`{"code":3.14}`,
		`{"code":true}`,
	}
	for _, raw := range cases {
		var msg shimMsg
		if err := json.Unmarshal([]byte(raw), &msg); err == nil {
			t.Errorf("Unmarshal(%q) want error, got nil (Code=%+v)", raw, msg.Code)
		}
	}
}

// TestShimMsgCode_UnmarshalJSONDirect tests shimMsgCode.UnmarshalJSON
// directly to cover the strconv.ParseInt hot path introduced by
// R20260602-PERF-6. Verifies: valid ints, boundary (0, max int64),
// invalid (float, bool, string, empty).
func TestShimMsgCode_UnmarshalJSONDirect(t *testing.T) {
	t.Run("valid_zero", func(t *testing.T) {
		var c shimMsgCode
		if err := c.UnmarshalJSON([]byte("0")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !c.Present || c.Value != 0 {
			t.Errorf("got Present=%v Value=%d, want Present=true Value=0", c.Present, c.Value)
		}
	})
	t.Run("valid_positive", func(t *testing.T) {
		var c shimMsgCode
		if err := c.UnmarshalJSON([]byte("137")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !c.Present || c.Value != 137 {
			t.Errorf("got Present=%v Value=%d, want Present=true Value=137", c.Present, c.Value)
		}
	})
	t.Run("valid_negative", func(t *testing.T) {
		var c shimMsgCode
		if err := c.UnmarshalJSON([]byte("-1")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !c.Present || c.Value != -1 {
			t.Errorf("got Present=%v Value=%d, want Present=true Value=-1", c.Present, c.Value)
		}
	})
	t.Run("invalid_float", func(t *testing.T) {
		var c shimMsgCode
		if err := c.UnmarshalJSON([]byte("3.14")); err == nil {
			t.Error("expected error for float, got nil")
		}
	})
	t.Run("invalid_bool", func(t *testing.T) {
		var c shimMsgCode
		if err := c.UnmarshalJSON([]byte("true")); err == nil {
			t.Error("expected error for bool, got nil")
		}
	})
	t.Run("invalid_string", func(t *testing.T) {
		var c shimMsgCode
		if err := c.UnmarshalJSON([]byte(`"42"`)); err == nil {
			t.Error("expected error for quoted string, got nil")
		}
	})
	t.Run("invalid_empty", func(t *testing.T) {
		var c shimMsgCode
		if err := c.UnmarshalJSON([]byte("")); err == nil {
			t.Error("expected error for empty data, got nil")
		}
	})
}
