package cli

// SinceInclusive converts a dashboard `after` cursor (unix ms of the last
// entry the client rendered) into the argument EntriesSince expects so the
// read is `Time >= after` instead of the strictly-greater `Time > after`.
//
// Same-millisecond siblings are legitimate (one CLI frame's thinking + text
// blocks, ACP's trailing thinking/text/result): a sibling appended after the
// client's last delivery shares the watermark ms and would otherwise never be
// replayed by any catch-up read (#2432, #2456). Every consumer of the
// inclusive read dedups same-ms replays by identity, so it is safe:
//   - dashboard.js appendEvents / onHistory / cron-live: uuid in the DOM
//   - dashboard.js scratch drawer renderNewEvents: scratchAdmitEvent (uuid
//     set at the watermark ms — its optimistic user bubble has no data-uuid)
//   - agent_view.js HTTP poll: dedupAgentPollBatch (content key; transcript
//     entries carry no uuid)
//   - scratch/handler.go context window: skips the exact-match entry itself
//
// after<=0 means "everything" and is unchanged.
//
// This is the one-shot (request/response) twin of SinceCursor
// (since_cursor.go), which does the same watermark-1 query plus uuid dedup
// for long-lived streaming loops that own their cursor.
func SinceInclusive(after int64) int64 {
	if after > 0 {
		return after - 1
	}
	return after
}
