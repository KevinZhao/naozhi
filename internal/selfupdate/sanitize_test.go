package selfupdate

import (
	"errors"
	"strings"
	"testing"
)

func TestSanitizeErrString(t *testing.T) {
	t.Run("strips signed-URL query credentials", func(t *testing.T) {
		// GitHub redirects release-asset downloads to a pre-signed
		// objects.githubusercontent.com URL. A wrapped *url.Error carries the
		// whole thing, so an ordinary download failure would paste live
		// (time-limited) credentials into the dashboard.
		in := `Get "https://objects.githubusercontent.com/gh/naozhi-darwin-arm64?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE&X-Amz-Signature=deadbeefcafe": dial tcp: i/o timeout`
		got := sanitizeErrString(in)
		for _, leak := range []string{"X-Amz-Signature", "X-Amz-Credential", "deadbeefcafe", "AKIAIOSFODNN7EXAMPLE"} {
			if strings.Contains(got, leak) {
				t.Errorf("sanitized error still contains %q:\n%s", leak, got)
			}
		}
		// The useful parts must survive — otherwise the operator learns nothing.
		if !strings.Contains(got, "objects.githubusercontent.com") {
			t.Errorf("host was dropped; the message should still say where it failed:\n%s", got)
		}
		if !strings.Contains(got, "i/o timeout") {
			t.Errorf("underlying cause was dropped:\n%s", got)
		}
	})

	t.Run("keeps a query-less URL intact", func(t *testing.T) {
		in := `fetch https://github.com/KevinZhao/naozhi/releases/download/v0.0.73/checksums.txt: 404`
		got := sanitizeErrString(in)
		if !strings.Contains(got, "releases/download/v0.0.73/checksums.txt") {
			t.Errorf("path was mangled on a URL with no query:\n%s", got)
		}
	})

	t.Run("redacts token-shaped env assignments", func(t *testing.T) {
		// launchctl/systemctl failures are reported via CombinedOutput, which
		// can echo the unit's environment.
		in := "launchctl kickstart failed\nGITHUB_TOKEN=ghp_supersecretvalue\nexit status 1"
		got := sanitizeErrString(in)
		if strings.Contains(got, "ghp_supersecretvalue") {
			t.Errorf("token value survived sanitization:\n%s", got)
		}
	})

	t.Run("collapses multi-line subprocess output to one line", func(t *testing.T) {
		got := sanitizeErrString("systemctl restart naozhi: exit 1\nJob failed.\nSee journalctl.")
		if strings.ContainsAny(got, "\n\r") {
			t.Errorf("sanitized error still contains newlines:\n%q", got)
		}
	})

	t.Run("preserves local paths", func(t *testing.T) {
		// Deliberately NOT scrubbed: "which directory was not writable" is the
		// single most useful thing an install failure can tell the operator.
		in := "chmod installed binary 0755: /usr/local/bin/naozhi: permission denied"
		got := sanitizeErrString(in)
		if !strings.Contains(got, "/usr/local/bin/naozhi") {
			t.Errorf("install path was scrubbed, which removes the actionable part:\n%s", got)
		}
	})

	t.Run("bounds the length", func(t *testing.T) {
		got := sanitizeErrString(strings.Repeat("x", maxErrRunes*3))
		// textutil.TruncateRunes appends a literal "..." (3 runes) when it trims.
		const limit = maxErrRunes + 3
		if n := len([]rune(got)); n > limit {
			t.Errorf("sanitized error is %d runes, want <= %d", n, limit)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if got := sanitizeErrString(""); got != "" {
			t.Errorf("sanitizeErrString(\"\") = %q, want empty", got)
		}
	})
}

func TestSanitizeErrNil(t *testing.T) {
	if got := sanitizeErr(nil); got != "" {
		t.Fatalf("sanitizeErr(nil) = %q, want empty", got)
	}
	if got := sanitizeErr(errors.New("boom")); got != "boom" {
		t.Fatalf("sanitizeErr(boom) = %q, want \"boom\"", got)
	}
}
