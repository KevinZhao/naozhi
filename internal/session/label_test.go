package session

import (
	"strings"
	"testing"
)

func TestValidateUserLabel(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty passes", "", "", false},
		{"whitespace-only passes", "   ", "", false},
		{"ASCII label", "hello", "hello", false},
		{"Chinese label", "重构会话", "重构会话", false},
		{"trims surrounding space", "  hi  ", "hi", false},
		{"128 bytes exact", strings.Repeat("a", MaxUserLabelBytes), strings.Repeat("a", MaxUserLabelBytes), false},
		{"129 bytes rejected", strings.Repeat("a", MaxUserLabelBytes+1), "", true},
		{"tab rejected (slog separator)", "a\tb", "", true},
		{"newline rejected", "a\nb", "", true},
		{"CR rejected", "a\rb", "", true},
		{"NUL rejected", "a\x00b", "", true},
		{"DEL rejected", "a\x7fb", "", true},
		{"C1 control rejected (U+0085 NEL)", "a\u0085b", "", true},
		{"C1 control rejected (U+009F)", "a\u009fb", "", true},
		{"escape rejected", "a\x1bb", "", true},
		{"invalid utf-8 rejected", "\xc3\x28", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateUserLabel(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("got = %q, want = %q", got, c.want)
			}
		})
	}
}

// TestSetUserLabel_NotifiesChange verifies that a label mutation triggers the
// onChange broadcast so connected dashboards refresh immediately instead of
// waiting up to one poll interval. R64-GO-H1 regression.
func TestSetUserLabel_NotifiesChange(t *testing.T) {
	r := newTestRouter(3)
	sk := "feishu:direct:test_user:default"
	injectSession(r, sk, nil)

	var notified int
	r.SetOnChange(func() { notified++ })

	if !r.SetUserLabel(sk, "custom") {
		t.Fatalf("SetUserLabel returned false for registered session")
	}
	if notified == 0 {
		t.Fatalf("expected onChange to fire after SetUserLabel, did not")
	}
	if got := r.GetSession(sk).UserLabel(); got != "custom" {
		t.Errorf("UserLabel = %q, want %q", got, "custom")
	}
}

// TestSetUserLabel_UnknownKeyNoNotify verifies that SetUserLabel on a missing
// key is a true no-op — no onChange fires, so the dashboard doesn't poll
// uselessly on typos from an RPC client. R64-GO-H1.
func TestSetUserLabel_UnknownKeyNoNotify(t *testing.T) {
	r := newTestRouter(3)
	var notified int
	r.SetOnChange(func() { notified++ })
	if r.SetUserLabel("missing:key:here:agent", "x") {
		t.Fatalf("SetUserLabel returned true for unknown key")
	}
	if notified != 0 {
		t.Fatalf("onChange fired %d times for unknown-key SetUserLabel", notified)
	}
}
