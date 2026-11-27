package cron

import (
	"encoding/json"
	"testing"
)

// TestIsJSONNull asserts our `null`-literal sniffer for transcriptTurn.Input
// — used to suppress wire `"input": null` that json.RawMessage's
// `omitempty` does not catch (R243-CR-P2-4 / #822).
func TestIsJSONNull(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"null", true},
		{" null", true},
		{"null\n", true},
		{"\tnull\r", true},
		{" null ", true},
		{"", false},
		{"NULL", false},
		{"nullx", false},
		{`"null"`, false},
		{`{}`, false},
		{`[]`, false},
		{`{"command":"ls"}`, false},
	}
	for _, c := range cases {
		got := isJSONNull(json.RawMessage(c.in))
		if got != c.want {
			t.Errorf("isJSONNull(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
