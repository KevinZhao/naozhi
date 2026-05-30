package cron

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestCronRunRecordBytesUpperBoundTracksFieldCaps pins R249-CR-28 (#967):
// MaxRunRecordBytes (32 KiB) must stay above the largest CronRun JSON a
// well-behaved finish path can produce. The worst-case record fills every
// string field to its documented cap THROUGH the same truncation helpers the
// finish path uses (Prompt ≤ MaxPromptBytes, Result via sanitiseRunResult →
// maxStoredResultRunes, ErrorMsg via sanitiseRunErrMsg → maxCronErrMsgRunes)
// and uses 4-byte runes for the rune-capped fields so the byte expansion is
// maximal. The test fails loudly if any field cap is bumped without raising
// MaxRunRecordBytes — the exact regression the godoc at limits.go:308 warns
// about but had no test guarding.
func TestCronRunRecordBytesUpperBoundTracksFieldCaps(t *testing.T) {
	// 4-byte UTF-8 rune (U+1F600) — maximises bytes-per-rune so the
	// rune-capped fields hit their largest possible byte footprint.
	const wideRune = "\U0001F600"

	// Prompt is byte-capped at MaxPromptBytes (not rune-capped). Fill to the
	// cap with a multi-byte rune that divides evenly into MaxPromptBytes.
	promptRunes := MaxPromptBytes / len(wideRune)
	prompt := strings.Repeat(wideRune, promptRunes)
	if len(prompt) > MaxPromptBytes {
		t.Fatalf("test setup: prompt %d bytes exceeds MaxPromptBytes %d", len(prompt), MaxPromptBytes)
	}

	// Result + ErrorMsg are rune-capped; push 2× their rune cap of 4-byte
	// runes through the real sanitise helpers so the output reflects exactly
	// what finishRun would persist (truncate + suffix + SanitizeForLog).
	rawResult := strings.Repeat(wideRune, maxStoredResultRunes*2)
	rawErr := strings.Repeat(wideRune, maxCronErrMsgRunes*2)

	run := &CronRun{
		RunID:       strings.Repeat("a", 64), // IsValidID upper bound
		JobID:       strings.Repeat("b", 64),
		State:       RunStateFailed,
		Trigger:     TriggerScheduled,
		StartedAt:   time.Now(),
		EndedAt:     time.Now().Add(90 * time.Minute),
		DurationMS:  90 * 60 * 1000,
		SessionID:   strings.Repeat("c", 64),
		Prompt:      prompt,
		WorkDir:     strings.Repeat("/d", 512), // generous absolute-path worst case
		Fresh:       true,
		Result:      sanitiseRunResult(rawResult),
		ResultBytes: 1 << 30,
		ErrorClass:  ErrClassWorkDirUnreachable,
		ErrorMsg:    sanitiseRunErrMsg(rawErr),
	}

	// Sanity-check the helpers actually capped the rune fields, otherwise the
	// upper-bound assertion below would be vacuously true.
	if got := utf8.RuneCountInString(run.Result); got > maxStoredResultRunes+utf8.RuneCountInString(truncatedSuffix) {
		t.Fatalf("Result not capped: %d runes > %d", got, maxStoredResultRunes)
	}
	if got := utf8.RuneCountInString(run.ErrorMsg); got > maxCronErrMsgRunes+utf8.RuneCountInString(truncatedSuffix) {
		t.Fatalf("ErrorMsg not capped: %d runes > %d", got, maxCronErrMsgRunes)
	}

	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if int64(len(data)) > MaxRunRecordBytes {
		t.Fatalf("worst-case CronRun JSON = %d bytes exceeds MaxRunRecordBytes %d; "+
			"a field cap (MaxPromptBytes=%d, maxStoredResultRunes=%d, maxCronErrMsgRunes=%d) "+
			"was raised without bumping MaxRunRecordBytes",
			len(data), MaxRunRecordBytes, MaxPromptBytes, maxStoredResultRunes, maxCronErrMsgRunes)
	}
	t.Logf("worst-case CronRun JSON = %d bytes (cap %d, headroom %d)",
		len(data), MaxRunRecordBytes, MaxRunRecordBytes-len(data))
}
