package project

import "testing"

// /project B in a chat bound to A must move the chat to B. Kept in both,
// bindingIndex picked A or B by map order and each Scan could flip it.
func TestBindChat_MovesChatFromOtherProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeProjectDir(t, root, "proj_a", nil)
	makeProjectDir(t, root, "proj_b", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}
	if err := m.BindChat("proj_a", "feishu", "group", "c1"); err != nil {
		t.Fatalf("BindChat(a) = %v", err)
	}
	if err := m.BindChat("proj_b", "feishu", "group", "c1"); err != nil {
		t.Fatalf("BindChat(b) = %v", err)
	}
	for i := 0; i < 30; i++ {
		if err := m.Scan(); err != nil {
			t.Fatalf("Scan = %v", err)
		}
		if p := m.ProjectForChat("feishu", "group", "c1"); p == nil || p.Name != "proj_b" {
			t.Fatalf("scan %d: ProjectForChat = %v, want proj_b", i, p)
		}
	}
	if n := len(m.Get("proj_a").Config.ChatBindings); n != 0 {
		t.Errorf("proj_a still holds %d binding(s), want 0", n)
	}

	// Persisted: a fresh manager reads the move from disk.
	m2, _ := NewManager(root, PlannerDefaults{})
	if err := m2.Scan(); err != nil {
		t.Fatalf("reload Scan = %v", err)
	}
	if n := len(m2.Get("proj_a").Config.ChatBindings); n != 0 {
		t.Errorf("after reload proj_a holds %d binding(s), want 0", n)
	}
	if p := m2.ProjectForChat("feishu", "group", "c1"); p == nil || p.Name != "proj_b" {
		t.Errorf("after reload ProjectForChat = %v, want proj_b", p)
	}
}

// A duplicate already on disk (hand edit) is cleared when the chat is bound
// again to one of the projects that hold it.
func TestBindChat_AlreadyBoundClearsDuplicateElsewhere(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dup := []ChatBinding{{Platform: "slack", ChatType: "group", ChatID: "c2"}}
	makeProjectDir(t, root, "proj_a", &ProjectConfig{ChatBindings: dup})
	makeProjectDir(t, root, "proj_b", &ProjectConfig{ChatBindings: dup})
	m, _ := NewManager(root, PlannerDefaults{})
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}
	if err := m.BindChat("proj_b", "slack", "group", "c2"); err != nil {
		t.Fatalf("BindChat = %v", err)
	}
	if n := len(m.Get("proj_a").Config.ChatBindings); n != 0 {
		t.Errorf("proj_a holds %d binding(s), want 0", n)
	}
	if n := len(m.Get("proj_b").Config.ChatBindings); n != 1 {
		t.Errorf("proj_b holds %d binding(s), want 1", n)
	}
}

// The settings modal PUTs back the config it loaded on open. A bind, unbind
// or star made meanwhile must survive its save.
func TestUpdateConfig_KeepsBindingsAndFavorite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeProjectDir(t, root, "proj", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}
	stale := m.Get("proj").Config // what the modal loaded: no bindings, not favorite

	if err := m.BindChat("proj", "feishu", "direct", "alice"); err != nil {
		t.Fatalf("BindChat = %v", err)
	}
	if err := m.SetFavorite("proj", true); err != nil {
		t.Fatalf("SetFavorite = %v", err)
	}

	stale.PlannerModel = "sonnet"
	if err := m.UpdateConfig("proj", stale); err != nil {
		t.Fatalf("UpdateConfig = %v", err)
	}
	p := m.Get("proj")
	if p.Config.PlannerModel != "sonnet" {
		t.Errorf("PlannerModel = %q, want sonnet", p.Config.PlannerModel)
	}
	if len(p.Config.ChatBindings) != 1 {
		t.Errorf("ChatBindings = %v, want the IM binding kept", p.Config.ChatBindings)
	}
	if !p.Config.Favorite {
		t.Error("Favorite reverted by the settings save")
	}
	if got := m.ProjectForChat("feishu", "direct", "alice"); got == nil || got.Name != "proj" {
		t.Errorf("ProjectForChat = %v, want proj", got)
	}

	// The reverse direction: an unbind made meanwhile must not come back.
	withBinding := m.Get("proj").Config
	if err := m.UnbindAllChat("feishu", "direct", "alice"); err != nil {
		t.Fatalf("UnbindAllChat = %v", err)
	}
	if err := m.UpdateConfig("proj", withBinding); err != nil {
		t.Fatalf("UpdateConfig = %v", err)
	}
	if got := m.ProjectForChat("feishu", "direct", "alice"); got != nil {
		t.Errorf("ProjectForChat = %q after unbind + stale save, want nil", got.Name)
	}
}
