package netutil

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		remoteAddr   string
		xff          string
		trustedProxy bool
		want         string
	}{
		{
			name:       "remote addr with port, no proxy",
			remoteAddr: "10.0.0.1:54321",
			want:       "10.0.0.1",
		},
		{
			name:       "bare remote addr, no proxy",
			remoteAddr: "10.0.0.1",
			want:       "10.0.0.1",
		},
		{
			name:         "trusted proxy reads last XFF entry",
			remoteAddr:   "10.0.0.1:54321",
			xff:          "1.2.3.4, 5.6.7.8, 9.10.11.12",
			trustedProxy: true,
			want:         "9.10.11.12",
		},
		{
			name:         "trusted proxy with single-entry XFF",
			remoteAddr:   "10.0.0.1:54321",
			xff:          "9.10.11.12",
			trustedProxy: true,
			want:         "9.10.11.12",
		},
		{
			name:         "trusted proxy with whitespace-padded XFF",
			remoteAddr:   "10.0.0.1:54321",
			xff:          "1.2.3.4,   9.10.11.12   ",
			trustedProxy: true,
			want:         "9.10.11.12",
		},
		{
			name:         "untrusted proxy ignores XFF",
			remoteAddr:   "10.0.0.1:54321",
			xff:          "evil.spoof",
			trustedProxy: false,
			want:         "10.0.0.1",
		},
		{
			name:         "malformed last XFF entry falls back to RemoteAddr",
			remoteAddr:   "10.0.0.1:54321",
			xff:          "1.2.3.4, not-an-ip",
			trustedProxy: true,
			want:         "10.0.0.1",
		},
		{
			name:         "empty XFF falls back to RemoteAddr",
			remoteAddr:   "10.0.0.1:54321",
			xff:          "",
			trustedProxy: true,
			want:         "10.0.0.1",
		},
		{
			name:         "IPv6 remote addr",
			remoteAddr:   "[2001:db8::1]:54321",
			trustedProxy: false,
			want:         "2001:db8::1",
		},
		{
			name:         "IPv6 last XFF entry",
			remoteAddr:   "10.0.0.1:54321",
			xff:          "1.2.3.4, 2001:db8::2",
			trustedProxy: true,
			want:         "2001:db8::2",
		},
		{
			name:         "XFF with only a comma falls back",
			remoteAddr:   "10.0.0.1:54321",
			xff:          ",",
			trustedProxy: true,
			want:         "10.0.0.1",
		},
		{
			name:         "XFF with comma and whitespace falls back",
			remoteAddr:   "10.0.0.1:54321",
			xff:          " , ",
			trustedProxy: true,
			want:         "10.0.0.1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			got := ClientIP(r, tc.trustedProxy)
			if got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
