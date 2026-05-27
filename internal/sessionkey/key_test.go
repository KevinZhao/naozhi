package sessionkey

import "testing"

func TestPrefixConstants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"CronKeyPrefix", CronKeyPrefix, "cron:"},
		{"SysKeyPrefix", SysKeyPrefix, "sys:"},
		{"ScratchKeyPrefix", ScratchKeyPrefix, "scratch:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestKeyConstructors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"CronKey/typical", CronKey("abc123"), "cron:abc123"},
		{"CronKey/empty id", CronKey(""), "cron:"},
		{"SysKey/typical", SysKey("auto_titler"), "sys:auto_titler"},
		{"SysKey/empty id", SysKey(""), "sys:"},
		{"ScratchKey/typical", ScratchKey("s_42"), "scratch:s_42"},
		{"ScratchKey/empty id", ScratchKey(""), "scratch:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestIsCronKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"cron:abc", true},
		{"cron:", true},
		{"cron:foo:bar", true}, // longer keys still match
		{"sys:abc", false},
		{"scratch:abc", false},
		{"feishu:group:t:a", false},
		{"", false},
		{"cron", false}, // missing colon
	}
	for _, tc := range cases {
		if got := IsCronKey(tc.in); got != tc.want {
			t.Errorf("IsCronKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsSysKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"sys:auto_titler", true},
		{"sys:", true},
		{"cron:auto_titler", false},
		{"scratch:auto_titler", false},
		{"", false},
		{"sys", false},
	}
	for _, tc := range cases {
		if got := IsSysKey(tc.in); got != tc.want {
			t.Errorf("IsSysKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsScratchKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"scratch:s_42", true},
		{"scratch:", true},
		{"cron:s_42", false},
		{"sys:s_42", false},
		{"", false},
		{"scratch", false},
	}
	for _, tc := range cases {
		if got := IsScratchKey(tc.in); got != tc.want {
			t.Errorf("IsScratchKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCronJobIDFromKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"cron:abc123", "abc123"},
		{"cron:", ""},                       // valid cron key with empty id → ""
		{"sys:abc123", ""},                  // wrong namespace → ""
		{"scratch:abc123", ""},              // wrong namespace → ""
		{"", ""},                            // empty input → ""
		{"abc123", ""},                      // missing prefix → ""
		{"cron:foo:bar:baz", "foo:bar:baz"}, // colons in id preserved
	}
	for _, tc := range cases {
		if got := CronJobIDFromKey(tc.in); got != tc.want {
			t.Errorf("CronJobIDFromKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestKeyConstructorRoundTrip confirms IsCronKey + CronJobIDFromKey are
// the inverse of CronKey.
func TestKeyConstructorRoundTrip(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"abc", "16-char-hex-here", "", "with:colon"} {
		key := CronKey(id)
		if !IsCronKey(key) {
			t.Errorf("CronKey(%q) = %q, IsCronKey returned false", id, key)
		}
		if got := CronJobIDFromKey(key); got != id {
			t.Errorf("round-trip CronKey/CronJobIDFromKey: id %q, got %q", id, got)
		}
	}
}
