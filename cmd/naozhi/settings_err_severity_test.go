package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
)

// TestClaudeSettingsErrSeverity pins the R236-QA-13 (#542) classification
// contract that main() relies on to route applyClaudeEnvSettings failures
// to the correct slog severity (Warn for missing/cancelled, Error for
// corrupt). Extracting the switch into a helper makes the routing logic
// testable without spinning up the full main() startup path.
func TestClaudeSettingsErrSeverity(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want settingsErrSeverity
	}{
		{
			name: "ctx canceled stays Warn (shutdown noise, not corruption)",
			err:  context.Canceled,
			want: settingsErrSeverityCancel,
		},
		{
			name: "ctx deadline exceeded stays Warn",
			err:  context.DeadlineExceeded,
			want: settingsErrSeverityCancel,
		},
		{
			name: "wrapped ctx canceled still classified as cancel",
			err:  fmt.Errorf("retry sleep: %w", context.Canceled),
			want: settingsErrSeverityCancel,
		},
		{
			name: "fs.ErrNotExist stays Warn (legitimate first-run state)",
			err:  fs.ErrNotExist,
			want: settingsErrSeverityMissing,
		},
		{
			name: "wrapped PathError(NotExist) classified as missing",
			err:  &os.PathError{Op: "open", Path: "/nope", Err: fs.ErrNotExist},
			want: settingsErrSeverityMissing,
		},
		{
			name: "corrupt-JSON parse error surfaces as Error severity",
			err:  errors.New("invalid JSON (attempt 3/3, 17 bytes)"),
			want: settingsErrSeverityFatal,
		},
		{
			name: "unrelated I/O error surfaces as Error severity",
			err:  errors.New("permission denied"),
			want: settingsErrSeverityFatal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeSettingsErrSeverity(tc.err)
			if got != tc.want {
				t.Fatalf("claudeSettingsErrSeverity(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
