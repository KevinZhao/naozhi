package cli

// api_frames.go — the claude frames that say how its model API calls went:
// system/api_retry and the result frame's timing (internal/apiwatch).

import (
	"encoding/json"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

type wireAPIRetry struct {
	Attempt      int    `json:"attempt"`
	MaxRetries   int    `json:"max_retries"`
	RetryDelayMS int64  `json:"retry_delay_ms"`
	ErrorStatus  *int   `json:"error_status"`
	Error        string `json:"error"`
	NoResponse   *struct {
		WaitedMS int64 `json:"waited_ms"`
	} `json:"no_response"`
}

// parseAPIRetry decodes a system/api_retry frame; nil when it does not decode.
func parseAPIRetry(line string) *clievent.APIRetry {
	var w wireAPIRetry
	if json.Unmarshal(stringToBytesUnsafe(line), &w) != nil {
		return nil
	}
	r := &clievent.APIRetry{Attempt: w.Attempt, MaxRetries: w.MaxRetries,
		RetryDelayMS: w.RetryDelayMS, Category: w.Error}
	if w.ErrorStatus != nil {
		r.Status = *w.ErrorStatus
	}
	if w.NoResponse != nil {
		r.WaitedMS = w.NoResponse.WaitedMS
	}
	return r
}

type wireAPITiming struct {
	DurationMS     int64 `json:"duration_ms"`
	DurationAPIMS  int64 `json:"duration_api_ms"`
	TTFTMS         int64 `json:"ttft_ms"`
	APIErrorStatus *int  `json:"api_error_status"`
}

// parseAPITiming decodes a result frame's timing; nil when it has none.
func parseAPITiming(line string) *clievent.APITiming {
	var w wireAPITiming
	if json.Unmarshal(stringToBytesUnsafe(line), &w) != nil {
		return nil
	}
	t := &clievent.APITiming{DurationMS: w.DurationMS, APIMS: w.DurationAPIMS, TTFTMS: w.TTFTMS}
	if w.APIErrorStatus != nil {
		t.ErrorStatus = *w.APIErrorStatus
	}
	if *t == (clievent.APITiming{}) {
		return nil
	}
	return t
}
