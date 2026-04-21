package main

import "testing"

func TestChatIDSuffix(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short_seven", "oc_abcde", "oc_abcde"}, // ≤8 bytes passes through
		{"exact_eight", "12345678", "12345678"},
		{"nine_chars", "123456789", "…23456789"},
		{"feishu_chat_id", "oc_9dcbfd8307c7a4c1e111f163aa47fd5d", "…aa47fd5d"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chatIDSuffix(tc.in); got != tc.want {
				t.Fatalf("chatIDSuffix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
