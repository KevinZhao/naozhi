// since_inclusive.go — event-pagination one-shot cursor conversion (#2649 G1-f).
//
// Moved out of internal/cli: pure cursor arithmetic over event timestamps,
// used by internal/server, internal/dashboard and internal/upstream. Nothing
// about it needs the process manager.
package clievent

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
