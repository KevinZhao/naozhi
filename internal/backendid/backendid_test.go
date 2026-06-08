package backendid

import "testing"

func TestIsValid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty allowed", "", true},
		{"lowercase", "claude", true},
		{"uppercase", "Claude", true},
		{"digits", "node01", true},
		{"dash underscore dot", "a-b_c.d", true},
		{"max len 64", string(make([]byte, MaxLen)), false}, // NUL bytes fail charset
		{"all dots at max", dots(MaxLen), true},
		{"over max len", dots(MaxLen + 1), false},
		{"space rejected", "a b", false},
		{"slash rejected", "a/b", false},
		{"colon rejected", "a:b", false},
		{"unicode rejected", "café", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValid(tt.in); got != tt.want {
				t.Errorf("IsValid(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestMaxLen(t *testing.T) {
	if MaxLen != 64 {
		t.Fatalf("MaxLen = %d, want 64 (router_backend.go maxBackendBytes contract)", MaxLen)
	}
}

func dots(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '.'
	}
	return string(b)
}
