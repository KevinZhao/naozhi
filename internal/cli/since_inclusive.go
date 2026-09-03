package cli

// SinceInclusive converts a dashboard `after` cursor (unix ms of the last
// entry the client rendered) into the argument EntriesSince expects so the
// read is `Time >= after` instead of the strictly-greater `Time > after`.
//
// Same-millisecond siblings are legitimate (one CLI frame's thinking + text
// blocks, ACP's trailing thinking/text/result): a sibling appended after the
// client's last delivery shares the watermark ms and would otherwise never be
// replayed by any catch-up read (#2432, #2456). The dashboard dedups same-ms
// replays by uuid (appendEvents / onHistory / cron-live), so the
// over-inclusive read is safe. after<=0 means "everything" and is unchanged.
func SinceInclusive(after int64) int64 {
	if after > 0 {
		return after - 1
	}
	return after
}
