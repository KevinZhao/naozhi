package session

import "testing"

// TestKeyResolver_ProjectBindingForChat pins the read-accessor contract
// dispatch slash-command UX paths (/cd, /new echo) rely on after
// R218B-ARCH-2 (#648) routed those reads through the resolver. Funnelling
// through KeyResolver eliminates the resolver-vs-projectMgr dual-info
// source race; the test asserts the three documented branches.
func TestKeyResolver_ProjectBindingForChat(t *testing.T) {
	t.Parallel()

	t.Run("nil_data_returns_unbound", func(t *testing.T) {
		t.Parallel()
		r := NewKeyResolver(nil, nil)
		got := r.ProjectBindingForChat("feishu", "direct", "u1")
		if got.Bound {
			t.Fatalf("nil data source must yield Bound=false, got %+v", got)
		}
	})

	t.Run("unbound_chat_returns_zero_value", func(t *testing.T) {
		t.Parallel()
		ds := &fakeDataSource{byChat: map[string]ProjectBinding{}}
		r := NewKeyResolver(nil, ds)
		got := r.ProjectBindingForChat("feishu", "direct", "u1")
		if got.Bound {
			t.Fatalf("unbound chat must yield Bound=false, got %+v", got)
		}
	})

	t.Run("bound_chat_passes_through_data_source", func(t *testing.T) {
		t.Parallel()
		want := ProjectBinding{
			Bound:        true,
			Name:         "demo",
			WorkspaceDir: "/srv/demo",
		}
		ds := &fakeDataSource{
			byChat: map[string]ProjectBinding{
				"feishu:direct:u1": want,
			},
		}
		r := NewKeyResolver(nil, ds)
		got := r.ProjectBindingForChat("feishu", "direct", "u1")
		if got != want {
			t.Fatalf("ProjectBindingForChat = %+v, want %+v", got, want)
		}
	})
}
