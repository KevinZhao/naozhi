package server

// User-facing 429 labels for the HTTP send path. They name the limiter that
// fired so the dashboard can display the body verbatim; before #2418's
// follow-up (F6) it collapsed every 429 into "消息队列已满", which none of
// these sources is.
const (
	sendRateLimitedMsg   = "发送过于频繁，请稍后重试"
	uploadRateLimitedMsg = "上传过于频繁，请稍后重试"
)
