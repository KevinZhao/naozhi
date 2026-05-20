package textutil

import (
	"testing"
	"time"
)

func TestFormatChineseDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "未知"},
		{-time.Second, "未知"},
		{2 * time.Hour, "2 小时"},
		{3 * time.Minute, "3 分钟"},
		{45 * time.Second, "45 秒"},
		{90 * time.Second, "1 分钟 30 秒"},
		{90 * time.Minute, "1 小时 30 分钟"},
		{time.Hour + time.Second, "1 小时"},
	}
	for _, tt := range tests {
		t.Run(tt.d.String(), func(t *testing.T) {
			if got := FormatChineseDuration(tt.d); got != tt.want {
				t.Errorf("FormatChineseDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}
