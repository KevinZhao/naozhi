package feishu

import (
	"strconv"
	"testing"
	"time"
)

// TestVerifyTimestamp pins the asymmetric freshness window: 5 min in the
// past, 30 s in the future. Added when the helpers moved out of feishu.go
// (R214-ARCH-13 split). Previously only verifySignature had direct unit
// coverage; verifyTimestamp was exercised transitively via webhook tests.
func TestVerifyTimestamp(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	tests := []struct {
		name      string
		timestamp string
		want      bool
	}{
		{"now", strconv.FormatInt(now, 10), true},
		{"1 min in past", strconv.FormatInt(now-60, 10), true},
		{"4 min in past (within 5min cap)", strconv.FormatInt(now-4*60, 10), true},
		{"6 min in past (beyond cap)", strconv.FormatInt(now-6*60, 10), false},
		{"10 s in future (within skew)", strconv.FormatInt(now+10, 10), true},
		{"30 s in future (boundary OK)", strconv.FormatInt(now+30, 10), true},
		{"60 s in future (beyond skew)", strconv.FormatInt(now+60, 10), false},
		{"non-numeric", "not-a-number", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := verifyTimestamp(tt.timestamp)
			if got != tt.want {
				t.Errorf("verifyTimestamp(%q) = %v, want %v", tt.timestamp, got, tt.want)
			}
		})
	}
}

// TestWindowConstants_NonceTTLAlignment asserts the static contract that
// the nonce-replay map's TTL covers the entire signature freshness window.
// If webhookTimestampMaxAge ever exceeds nonceTTL, an attacker could replay
// a webhook payload after its nonce had been swept from seenNonces but
// before verifyTimestamp would reject the timestamp — silently re-opening
// the replay hole that R235-SEC-9 closed. R214-ARCH-13.
func TestWindowConstants_NonceTTLAlignment(t *testing.T) {
	t.Parallel()
	if int64(nonceTTL.Seconds()) < webhookTimestampMaxAge {
		t.Fatalf("nonceTTL=%v < webhookTimestampMaxAge=%ds — replay window opens",
			nonceTTL, webhookTimestampMaxAge)
	}
}
