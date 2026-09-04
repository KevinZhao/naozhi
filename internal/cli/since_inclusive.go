package cli

// SinceInclusive converts a dashboard `after` cursor (unix ms of the last
// entry the client rendered) into the EntriesSince argument that reads
// `Time >= after` instead of `Time > after`.
//
// Same-millisecond siblings are real (one CLI frame's thinking + text blocks,
// ACP's trailing thinking/text/result); one appended after the client's last
// delivery shares the watermark ms and would otherwise never be replayed
// (#2432, #2456). Every consumer dedups same-ms replays by identity (uuid in
// DOM, scratchAdmitEvent, dedupAgentPollBatch, scratch/handler.go exact-match
// skip). after<=0 means "everything". SinceCursor is the streaming twin.
func SinceInclusive(after int64) int64 {
	if after > 0 {
		return after - 1
	}
	return after
}
