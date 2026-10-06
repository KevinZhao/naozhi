package config

import (
	"reflect"
	"testing"
)

func loadBody(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, body)
	}
	return cfg
}

func TestReloadDiff(t *testing.T) {
	t.Parallel()
	base := "cli:\n  model: sonnet\nlog:\n  level: info\n  format: json\n"
	cases := []struct {
		name          string
		next          string
		wantHot, want []string
	}{
		{"identical", base, nil, nil},
		{"log.level only", "cli:\n  model: sonnet\nlog:\n  level: debug\n  format: json\n", []string{"log.level"}, nil},
		{"log.format", "cli:\n  model: sonnet\nlog:\n  level: info\n  format: text\n", nil, []string{"log"}},
		{"cli.model", "cli:\n  model: opus\nlog:\n  level: info\n  format: json\n", nil, []string{"cli"}},
		{"im_access added", base + "im_access:\n  platforms:\n    feishu:\n      allowed_users: [ou_1]\n", []string{"im_access"}, nil},
		{"im_rate_limit added", base + "im_rate_limit:\n  msgs_per_min: 5\n", []string{"im_rate_limit"}, nil},
		{"mixed", "cli:\n  model: opus\nlog:\n  level: warn\n  format: text\nim_rate_limit:\n  msgs_per_min: 5\n",
			[]string{"im_rate_limit", "log.level"}, []string{"cli", "log"}},
	}
	cur := loadBody(t, base)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := loadBody(t, tc.next)
			if got := cur.HotChanged(next); !reflect.DeepEqual(got, tc.wantHot) {
				t.Errorf("HotChanged = %v, want %v", got, tc.wantHot)
			}
			if got := cur.RestartRequired(next); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("RestartRequired = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every hot section is a real yaml key, and none of them is also counted as
// restart-required.
func TestHotSections_AreYAMLKeys(t *testing.T) {
	t.Parallel()
	var names []string
	typ := reflect.TypeFor[Config]()
	for i := 0; i < typ.NumField(); i++ {
		if n := yamlName(typ.Field(i)); n != "" {
			names = append(names, n)
		}
	}
	for _, h := range HotSections() {
		key, _, _ := cutDot(h)
		found := false
		for _, n := range names {
			if n == key {
				found = true
			}
		}
		if !found {
			t.Errorf("hot section %q is not a Config yaml key", h)
		}
	}
	if len(names) < 10 {
		t.Fatalf("yamlName found only %d named sections: %v", len(names), names)
	}
}

func cutDot(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// IMAccessOpened names a running platform only when the previous file
// restricted it and the next one serves every sender: a misspelt im_access
// key decodes as absent, so the reload silently opens the platform.
func TestIMAccessOpened(t *testing.T) {
	t.Parallel()
	slackRule := "im_access:\n  platforms:\n    slack:\n      allowed_users: [U1]\n"
	cases := []struct {
		name, prev, next string
		want             []string
	}{
		{"misspelt key opens", slackRule, "im_acess:\n  platforms:\n    slack:\n      allowed_users: [U1]\n", []string{"slack"}},
		{"default_deny dropped", "im_access:\n  default_deny: true\n", "", []string{"slack"}},
		{"rule kept", slackRule, slackRule, nil},
		{"rule swapped for default_deny", slackRule, "im_access:\n  default_deny: true\n", nil},
		{"already open", "", "", nil},
		{"rule for another platform only", slackRule, "im_access:\n  platforms:\n    feishu:\n      allowed_users: [ou_1]\n", []string{"slack"}},
	}
	running := loadBody(t, imAccessSlack)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev, next := loadBody(t, imAccessSlack+tc.prev), loadBody(t, imAccessSlack+tc.next)
			if got := running.IMAccessOpened(prev, next); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("IMAccessOpened = %v, want %v", got, tc.want)
			}
		})
	}
}
