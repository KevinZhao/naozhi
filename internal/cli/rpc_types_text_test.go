package cli

import (
	"encoding/json"
	"testing"
)

func TestRPCErrorText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		e    RPCError
		want string
	}{
		{"no data", RPCError{Message: "model overloaded"}, "model overloaded"},
		{"string data", RPCError{Message: "Internal error", Data: json.RawMessage(`"model unavailable"`)}, "Internal error: model unavailable"},
		{"string data without message", RPCError{Data: json.RawMessage(`"model unavailable"`)}, "model unavailable"},
		{"empty string data", RPCError{Message: "Internal error", Data: json.RawMessage(`""`)}, "Internal error"},
		{"null data", RPCError{Message: "Internal error", Data: json.RawMessage(`null`)}, "Internal error"},
		{"structured data is left out", RPCError{Message: "Internal error", Data: json.RawMessage(`{"retry":true}`)}, "Internal error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.e.Text(); got != tc.want {
				t.Errorf("Text() = %q, want %q", got, tc.want)
			}
		})
	}
}
