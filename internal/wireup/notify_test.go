package wireup

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
)

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

// A session never named reads as its latest prompt's first sentence, as the
// dashboard titles it, not as the CLI's --name (the workspace directory).
func TestUnitNamesLocal(t *testing.T) {
	r := session.NewRouter(session.RouterConfig{})
	t.Cleanup(r.Shutdown)
	name := UnitNames(r, nil, nil)
	tests := []struct{ key, prompt, want string }{
		{"dashboard:direct:a:general", "压测一下网关。跑完告诉我结果", "压测一下网关"},
		{"dashboard:direct:b:general", "跑 `sleep 300` 看看\n第二行", "跑 看看"},
		{"dashboard:direct:c:general", "把这一大段很长很长很长很长的需求一次性全部整理成一份清单", "把这一大段很长很长很长很长的需求一次性全..."},
	}
	for _, tt := range tests {
		r.InjectSession(tt.key, session.NewTestProcess()).InjectHistory([]clievent.EventEntry{{Type: clievent.KindUser, Detail: tt.prompt, Summary: tt.prompt, Time: 1}})
		if got := name(tt.key); got != tt.want {
			t.Errorf("name(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}
