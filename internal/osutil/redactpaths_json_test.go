package osutil

import (
	"encoding/json"
	"testing"
)

// TestRedactAbsolutePaths_JSONEscapedQuotesKeepJSONValid pins the dashboard
// "200 empty body" regression: RedactAbsolutePaths runs over raw NDJSON
// lines (cron sandbox run events). A POSIX path followed by a JSON-escaped
// quote (`\"/tmp/f\"`) used to swallow the backslash as a path byte, leaving
// `"<path>""` — invalid JSON that json.Encoder refuses to emit.
func TestRedactAbsolutePaths_JSONEscapedQuotesKeepJSONValid(t *testing.T) {
	in := `{"cmd":"cat \"/tmp/f\""}`
	got := RedactAbsolutePaths(in)
	if !json.Valid([]byte(got)) {
		t.Fatalf("RedactAbsolutePaths broke JSON:\n in  = %s\n got = %s", in, got)
	}
	if want := `{"cmd":"cat \"<path>\""}`; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// A backslash that does NOT escape a quote stays part of the POSIX token
	// (plain-text callers such as cron IM notices rely on whole-token masking).
	if got := RedactAbsolutePaths(`open /home/u/x\ y/z: denied`); got != "open <path> y<path>: denied" {
		t.Errorf("backslash-space path: %q", got)
	}
	if got := RedactAbsolutePaths(`{"m":"open /tmp/a\nb"}`); got != `{"m":"open <path>"}` {
		t.Errorf("escaped newline after path: %q", got)
	}
	// Windows drive paths still consume their backslash separators.
	if got := RedactAbsolutePaths(`open C:\Users\bob\f: denied`); got != "open <path>: denied" {
		t.Errorf("windows path regressed: %q", got)
	}
}
