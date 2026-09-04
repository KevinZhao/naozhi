package textutil

import (
	"fmt"
	"time"
)

// FormatChineseDuration formats a duration as a short Chinese string using
// the largest meaningful unit pair (90m → "1 小时 30 分钟"); round values
// collapse to a single unit. Returns "未知" for zero or negative durations
// so IM banners / cron notices get a deterministic placeholder.
func FormatChineseDuration(d time.Duration) string {
	if d <= 0 {
		return "未知"
	}
	if d >= time.Hour {
		h := int(d / time.Hour)
		rem := d - time.Duration(h)*time.Hour
		m := int(rem / time.Minute)
		if m == 0 {
			return fmt.Sprintf("%d 小时", h)
		}
		return fmt.Sprintf("%d 小时 %d 分钟", h, m)
	}
	if d >= time.Minute {
		m := int(d / time.Minute)
		rem := d - time.Duration(m)*time.Minute
		s := int(rem / time.Second)
		if s == 0 {
			return fmt.Sprintf("%d 分钟", m)
		}
		return fmt.Sprintf("%d 分钟 %d 秒", m, s)
	}
	return fmt.Sprintf("%d 秒", int(d.Seconds()))
}
