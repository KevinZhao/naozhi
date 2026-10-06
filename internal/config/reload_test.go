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
		{"im_limits added", base + "im_limits:\n  per_chat_daily_usd: 5\n", []string{"im_limits"}, nil},
		{"mixed", "cli:\n  model: opus\nlog:\n  level: warn\n  format: text\nim_limits:\n  per_chat_daily_usd: 5\n",
			[]string{"im_limits", "log.level"}, []string{"cli", "log"}},
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
