package wireup

import "testing"

func TestUnitNamesRemote(t *testing.T) {
	remote := func() (map[string][]map[string]any, map[string]string) {
		return map[string][]map[string]any{"mini": {
			{"key": "dashboard:direct:x:general", "user_label": "Fix CI", "summary": "ci"},
			{"key": "dashboard:direct:y:general", "summary": "Write docs"},
		}}, map[string]string{"mini": "ok"}
	}
	name := UnitNames(nil, nil, remote)
	tests := []struct{ unit, want string }{
		{"mini/dashboard:direct:x:general", "Fix CI"},
		{"mini/dashboard:direct:y:general", "Write docs"},
		{"mini/dashboard:direct:z:general", ""},
		{"box/dashboard:direct:x:general", ""},
		{"dashboard:direct:x:general", ""},
	}
	for _, tt := range tests {
		if got := name(tt.unit); got != tt.want {
			t.Errorf("name(%q) = %q, want %q", tt.unit, got, tt.want)
		}
	}
}
