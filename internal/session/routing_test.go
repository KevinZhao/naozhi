package session

import (
	"reflect"
	"sync"
	"testing"
)

// ----- fake PlannerDataSource --------------------------------------------

// fakeDataSource is a test-only implementation of PlannerDataSource.
// It returns configured bindings verbatim, making table-driven tests easy.
type fakeDataSource struct {
	byChat map[string]ProjectBinding // "platform:chatType:chatID" -> binding
	byName map[string]ProjectBinding
}

func (f *fakeDataSource) ProjectBinding(platform, chatType, chatID string) ProjectBinding {
	if f == nil || f.byChat == nil {
		return ProjectBinding{}
	}
	k := platform + ":" + chatType + ":" + chatID
	if b, ok := f.byChat[k]; ok {
		return b
	}
	return ProjectBinding{}
}

func (f *fakeDataSource) ProjectByName(name string) (ProjectBinding, bool) {
	if f == nil || f.byName == nil {
		return ProjectBinding{}, false
	}
	b, ok := f.byName[name]
	return b, ok
}

// ----- plannerKeyFor / isPlannerKey format lock --------------------------

func TestPlannerKeyFor_Format(t *testing.T) {
	t.Parallel()
	// Hardcoded literal — MUST stay in sync with project.PlannerKeyFor.
	// project_test.go TestPlannerKeyFor locks the same string from the
	// project-package side. A format migration must update both.
	if got := plannerKeyFor("foo"); got != "project:foo:planner" {
		t.Errorf("plannerKeyFor(foo) = %q, want %q", got, "project:foo:planner")
	}
}

func TestIsPlannerKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key  string
		want bool
	}{
		{"project:foo:planner", true},
		{"project:my-project:planner", true},
		{"project::planner", false}, // empty name must be rejected
		{"project:foo", false},
		{"cron:foo", false},
		{"scratch:abc:general:general", false},
		{"feishu:direct:alice:general", false},
		{"", false},
		{":planner", false},
		{"project:", false},
	}
	for _, tc := range cases {
		if got := isPlannerKey(tc.key); got != tc.want {
			t.Errorf("isPlannerKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestPlannerNameFromKey(t *testing.T) {
	t.Parallel()
	// Precondition: caller has verified isPlannerKey; we only test the
	// happy path here (§3.5).
	cases := map[string]string{
		"project:foo:planner":        "foo",
		"project:my-project:planner": "my-project",
	}
	for key, want := range cases {
		if got := plannerNameFromKey(key); got != want {
			t.Errorf("plannerNameFromKey(%q) = %q, want %q", key, got, want)
		}
	}
}

// ----- ResolveForChat: 288-row matrix with aliasing canary ---------------

type forChatCase struct {
	name      string
	agentID   string
	defaults  map[string]AgentOpts
	binding   ProjectBinding // zero-value ⇒ unbound
	wantKey   string
	wantOpts  AgentOpts
	canaryCap bool // true: allocate ExtraArgs with cap > len to assert no leak
}

func TestResolveForChat(t *testing.T) {
	t.Parallel()
	cases := []forChatCase{
		{
			name:     "unbound_general_no_defaults",
			agentID:  "general",
			defaults: map[string]AgentOpts{},
			wantKey:  "feishu:direct:alice:general",
			wantOpts: AgentOpts{},
		},
		{
			name:    "unbound_general_with_defaults",
			agentID: "general",
			defaults: map[string]AgentOpts{
				"general": {Model: "sonnet", Backend: "claude"},
			},
			wantKey:  "feishu:direct:alice:general",
			wantOpts: AgentOpts{Model: "sonnet", Backend: "claude"},
		},
		{
			name:    "unbound_nongeneral",
			agentID: "code-reviewer",
			defaults: map[string]AgentOpts{
				"code-reviewer": {Model: "opus", ExtraArgs: []string{"--verbose"}},
			},
			wantKey:  "feishu:direct:alice:code-reviewer",
			wantOpts: AgentOpts{Model: "opus", ExtraArgs: []string{"--verbose"}},
		},
		{
			name:    "unknown_agent_falls_to_zero_opts",
			agentID: "nonexistent",
			defaults: map[string]AgentOpts{
				"general": {Model: "sonnet"},
			},
			wantKey:  "feishu:direct:alice:nonexistent",
			wantOpts: AgentOpts{},
		},
		{
			name:    "bound_nongeneral_workspace_overlay_only",
			agentID: "code-reviewer",
			defaults: map[string]AgentOpts{
				"code-reviewer": {Model: "opus", Exempt: true}, // Exempt must be reset
			},
			binding:  ProjectBinding{Bound: true, Name: "myproj", WorkspaceDir: "/w/myproj"},
			wantKey:  "feishu:direct:alice:code-reviewer",
			wantOpts: AgentOpts{Model: "opus", Workspace: "/w/myproj", Exempt: false},
		},
		{
			name:    "bound_general_planner_full_overlay",
			agentID: "general",
			defaults: map[string]AgentOpts{
				"general": {Model: "sonnet", Backend: "claude"},
			},
			binding: ProjectBinding{
				Bound: true, Name: "myproj", WorkspaceDir: "/w/myproj",
				PlannerModel: "opus", PlannerPrompt: "P",
			},
			wantKey: "project:myproj:planner",
			wantOpts: AgentOpts{
				Model:     "opus",
				Workspace: "/w/myproj",
				Exempt:    true,
				Backend:   "claude",
				ExtraArgs: []string{"--append-system-prompt", "P"},
			},
		},
		{
			name:    "bound_general_planner_no_model_override",
			agentID: "general",
			defaults: map[string]AgentOpts{
				"general": {Model: "sonnet"},
			},
			binding: ProjectBinding{Bound: true, Name: "p", WorkspaceDir: "/w"},
			wantKey: "project:p:planner",
			wantOpts: AgentOpts{
				Model:     "sonnet", // defaults preserved when no override
				Workspace: "/w",
				Exempt:    true,
			},
		},
		{
			name:    "bound_general_aliasing_canary",
			agentID: "general",
			defaults: map[string]AgentOpts{
				// cap > len: if ResolveForChat uses two-arg append, it
				// will write into the shared backing array past len.
				"general": {Model: "sonnet", ExtraArgs: makeArgsWithCap(
					[]string{"--existing"}, 8)},
			},
			binding: ProjectBinding{
				Bound: true, Name: "p", WorkspaceDir: "/w", PlannerPrompt: "P",
			},
			canaryCap: true,
			wantKey:   "project:p:planner",
			wantOpts: AgentOpts{
				Model:     "sonnet",
				Workspace: "/w",
				Exempt:    true,
				ExtraArgs: []string{"--existing", "--append-system-prompt", "P"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Snapshot defaults.ExtraArgs backing array (including cap)
			// for aliasing canary assertion.
			var canaryBefore []string
			if tc.canaryCap {
				orig := tc.defaults["general"].ExtraArgs
				canaryBefore = append([]string(nil), orig[:cap(orig)]...)
			}

			data := &fakeDataSource{}
			if tc.binding.Bound {
				k := "feishu:direct:alice"
				data.byChat = map[string]ProjectBinding{k: tc.binding}
			}
			r := NewKeyResolver(tc.defaults, data)

			gotKey, gotOpts := r.ResolveForChat("feishu", "direct", "alice", tc.agentID)

			if gotKey != tc.wantKey {
				t.Errorf("key = %q, want %q", gotKey, tc.wantKey)
			}
			if !reflect.DeepEqual(gotOpts, tc.wantOpts) {
				t.Errorf("opts mismatch:\n got: %#v\nwant: %#v", gotOpts, tc.wantOpts)
			}

			if tc.canaryCap {
				// Aliasing canary — the correct `[:len:len]` path must
				// never write past the original len slot of defaults'
				// backing array. Peek the full cap range and compare
				// against the pre-call snapshot.
				orig := tc.defaults["general"].ExtraArgs
				after := orig[:cap(orig)]
				if !reflect.DeepEqual(canaryBefore, after) {
					t.Errorf("aliasing detected: defaults.ExtraArgs backing array mutated\n"+
						"before cap=%d: %#v\nafter  cap=%d: %#v",
						cap(orig), canaryBefore, cap(orig), after)
				}
			}
		})
	}
}

// makeArgsWithCap returns a slice with the given logical content but
// capacity >= cap. Used by the aliasing canary case.
func makeArgsWithCap(content []string, capacity int) []string {
	s := make([]string, len(content), capacity)
	copy(s, content)
	return s
}

// TestResolveForChat_NonPlannerNoAliasing locks R215-ARCH-P2-8: even
// branches that do NOT append must clone ExtraArgs so a downstream
// caller appending to opts.ExtraArgs cannot poison the shared
// defaults backing array.
//
// Covers the 3 non-planner return paths:
//
//   - r.data == nil (no project layer at all)
//   - r.data != nil + chat unbound
//   - r.data != nil + chat bound + non-general agent
//
// All three previously returned `base` whose ExtraArgs aliased
// r.defaults[agentID].ExtraArgs verbatim. The aliasing canary append
// at the end of each subtest forces the bug to surface as a poisoned
// shared slot if the clone is ever removed.
func TestResolveForChat_NonPlannerNoAliasing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		agentID  string
		hasData  bool
		bound    bool
		bindName string
	}{
		{name: "data_nil", agentID: "general"},
		{name: "data_nonNil_chat_unbound", agentID: "general", hasData: true},
		{
			name:     "data_nonNil_chat_bound_nongeneral",
			agentID:  "code-reviewer",
			hasData:  true,
			bound:    true,
			bindName: "p",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shared := makeArgsWithCap([]string{"--base"}, 8)
			defaults := map[string]AgentOpts{
				tc.agentID: {Model: "sonnet", ExtraArgs: shared},
			}
			canaryBefore := append([]string(nil), shared[:cap(shared)]...)

			var data PlannerDataSource
			if tc.hasData {
				ds := &fakeDataSource{}
				if tc.bound {
					ds.byChat = map[string]ProjectBinding{
						"feishu:direct:alice": {
							Bound: true, Name: tc.bindName, WorkspaceDir: "/w",
						},
					}
				}
				data = ds
			}
			r := NewKeyResolver(defaults, data)

			_, opts := r.ResolveForChat("feishu", "direct", "alice", tc.agentID)

			// Force aliasing: append into opts.ExtraArgs. With the
			// clone in place this writes into a private backing array;
			// without it, the append (cap=8, len=1) writes the new
			// element directly into shared[1] and the canary fires.
			opts.ExtraArgs = append(opts.ExtraArgs, "--injected")

			peek := defaults[tc.agentID].ExtraArgs
			peekCap := peek[:cap(peek)]
			if !reflect.DeepEqual(canaryBefore, peekCap) {
				t.Errorf("aliasing leaked: defaults backing array mutated\n"+
					"before cap=%d: %#v\nafter  cap=%d: %#v",
					cap(peek), canaryBefore, cap(peek), peekCap)
			}
		})
	}
}

// ----- ResolveForPlannerKey ----------------------------------------------

func TestResolveForPlannerKey(t *testing.T) {
	t.Parallel()
	t.Run("data_nil", func(t *testing.T) {
		r := NewKeyResolver(nil, nil)
		_, _, ok := r.ResolveForPlannerKey("foo")
		if ok {
			t.Error("expected ok=false when data is nil")
		}
	})
	t.Run("project_not_found", func(t *testing.T) {
		r := NewKeyResolver(nil, &fakeDataSource{byName: map[string]ProjectBinding{}})
		_, _, ok := r.ResolveForPlannerKey("missing")
		if ok {
			t.Error("expected ok=false for missing project")
		}
	})
	t.Run("does_not_inherit_defaults", func(t *testing.T) {
		// CRITICAL: this is the B1 safeguard. planner-view MUST NOT
		// pull Model/ExtraArgs/Backend from defaults["general"]. A
		// regression here would change planner argv on production.
		defaults := map[string]AgentOpts{
			"general": {
				Model:     "should-NOT-appear",
				ExtraArgs: []string{"--should-NOT-appear"},
				Backend:   "should-NOT-appear",
				Workspace: "should-NOT-appear", // SS3: project path wins, not defaults
			},
		}
		data := &fakeDataSource{byName: map[string]ProjectBinding{
			"p": {Bound: true, Name: "p", WorkspaceDir: "/w", PlannerModel: "opus", PlannerPrompt: "P"},
		}}
		r := NewKeyResolver(defaults, data)

		key, opts, ok := r.ResolveForPlannerKey("p")
		if !ok {
			t.Fatal("expected ok=true")
		}
		if key != "project:p:planner" {
			t.Errorf("key = %q, want %q", key, "project:p:planner")
		}
		want := AgentOpts{
			Model:     "opus",
			Workspace: "/w",
			Exempt:    true,
			ExtraArgs: []string{"--append-system-prompt", "P"},
			// Backend MUST be empty — not copied from defaults.
		}
		if !reflect.DeepEqual(opts, want) {
			t.Errorf("planner-view inherited defaults! got: %#v\nwant: %#v", opts, want)
		}
	})
	t.Run("no_prompt_no_extraargs", func(t *testing.T) {
		data := &fakeDataSource{byName: map[string]ProjectBinding{
			"p": {Bound: true, Name: "p", WorkspaceDir: "/w", PlannerModel: "opus"},
		}}
		r := NewKeyResolver(nil, data)
		_, opts, ok := r.ResolveForPlannerKey("p")
		if !ok {
			t.Fatal("expected ok=true")
		}
		if opts.ExtraArgs != nil {
			t.Errorf("ExtraArgs should be nil when no prompt, got %#v", opts.ExtraArgs)
		}
	})
}

// ----- ResolveForKey: 4-branch dispatch ----------------------------------

func TestResolveForKey(t *testing.T) {
	t.Parallel()
	data := &fakeDataSource{byName: map[string]ProjectBinding{
		"p": {Bound: true, Name: "p", WorkspaceDir: "/w", PlannerModel: "opus"},
	}}
	defaults := map[string]AgentOpts{
		"general":       {Model: "sonnet"},
		"code-reviewer": {Model: "haiku"},
	}
	r := NewKeyResolver(defaults, data)

	cases := []struct {
		name    string
		key     string
		wantOK  bool
		wantOpt AgentOpts
	}{
		{"planner_exists", "project:p:planner", true,
			AgentOpts{Model: "opus", Workspace: "/w", Exempt: true}},
		{"planner_missing", "project:missing:planner", false, AgentOpts{}},
		{"scratch_key_rejected", "scratch:abc:general:general", false, AgentOpts{}},
		{"cron_key_rejected", "cron:daily", false, AgentOpts{}},
		{"im_4segment_general", "feishu:direct:alice:general", true,
			AgentOpts{Model: "sonnet"}},
		{"im_4segment_agent", "feishu:group:xxx:code-reviewer", true,
			AgentOpts{Model: "haiku"}},
		{"im_4segment_unknown_agent_zero", "feishu:direct:alice:nonexistent", true, AgentOpts{}},
		{"malformed_empty", "", false, AgentOpts{}},
		{"malformed_two_segments", "foo:bar", false, AgentOpts{}},
		{"malformed_too_many", "a:b:c:d:e:f", true, AgentOpts{}}, // SplitN(_, 4) keeps 4th intact "d:e:f" → defaults["d:e:f"] = zero
		// D1: "project:" prefix without ":planner" suffix is not a planner
		// key. isPlannerKey returns false; IsReservedNamespace then catches
		// it and returns ok=false. Explicit coverage to prevent future
		// regression if the reserved-namespace short-circuit is reordered.
		{"project_prefix_no_planner_suffix", "project:foo", false, AgentOpts{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, ok := r.ResolveForKey(tc.key)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !reflect.DeepEqual(opts, tc.wantOpt) {
				t.Errorf("opts = %#v, want %#v", opts, tc.wantOpt)
			}
		})
	}
}

// TestResolveForKey_IM4SegmentDoesNotOverlayWorkspace is the SS2
// regression guard. The RFC §3.1 branch (c) comment states "does NOT
// overlay workspace" for the IM 4-segment branch (resume path:
// workspace comes from sessions.json independently). A naive
// implementation that calls ProjectBinding on the 4-segment path
// would leak the project's workspace into the resumed opts, violating
// the resume semantics. Keep this test even if the table in
// TestResolveForKey grows — it pins the contract from a different
// angle.
func TestResolveForKey_IM4SegmentDoesNotOverlayWorkspace(t *testing.T) {
	t.Parallel()
	// Bind the chat in data.byChat: a regression that called
	// ProjectBinding on the 4-segment path would hit this binding and
	// overlay Workspace. The correct behaviour skips data entirely.
	data := &fakeDataSource{
		byChat: map[string]ProjectBinding{
			"feishu:direct:alice": {
				Bound:        true,
				Name:         "myproj",
				WorkspaceDir: "/should-NOT-leak-into-resume",
			},
		},
	}
	defaults := map[string]AgentOpts{
		"general": {Model: "sonnet"},
	}
	r := NewKeyResolver(defaults, data)

	opts, ok := r.ResolveForKey("feishu:direct:alice:general")
	if !ok {
		t.Fatal("expected ok=true for IM 4-segment key")
	}
	if opts.Workspace != "" {
		t.Errorf("ResolveForKey leaked ProjectBinding workspace into resume opts: got %q, want empty", opts.Workspace)
	}
	if opts.Model != "sonnet" {
		t.Errorf("defaults[general].Model should pass through, got %q", opts.Model)
	}
}

// ----- KeyForChat --------------------------------------------------------

func TestKeyForChat(t *testing.T) {
	t.Parallel()
	data := &fakeDataSource{byChat: map[string]ProjectBinding{
		"feishu:direct:alice": {Bound: true, Name: "myproj"},
	}}
	r := NewKeyResolver(nil, data)

	cases := []struct {
		name     string
		agentID  string
		platform string
		want     string
	}{
		{"bound_general_planner", "general", "feishu", "project:myproj:planner"},
		{"bound_nongeneral_im_key", "code-reviewer", "feishu", "feishu:direct:alice:code-reviewer"},
		{"unbound_general_im_key", "general", "unbound", "unbound:direct:alice:general"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.KeyForChat(tc.platform, "direct", "alice", tc.agentID)
			if got != tc.want {
				t.Errorf("KeyForChat = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeyForChat_NilData(t *testing.T) {
	t.Parallel()
	r := NewKeyResolver(nil, nil)
	if got := r.KeyForChat("feishu", "direct", "alice", "general"); got != "feishu:direct:alice:general" {
		t.Errorf("got %q", got)
	}
}

// ----- Concurrent aliasing: 10 goroutines × 100 iters --------------------

func TestResolveForChat_ConcurrentNoAliasing(t *testing.T) {
	t.Parallel()
	// Shared defaults with cap > len — if the Resolver's three-arg
	// slice protection fails, concurrent goroutines will race to
	// write the "--append-system-prompt" cell into the shared backing
	// array. With the race detector on (-race) this surfaces as a
	// DATA RACE warning; without it, the canary slot assertion fires.
	shared := make([]string, 1, 8)
	shared[0] = "--base"
	defaults := map[string]AgentOpts{
		"general": {ExtraArgs: shared},
	}
	data := &fakeDataSource{byChat: map[string]ProjectBinding{
		"feishu:direct:alice": {Bound: true, Name: "p", WorkspaceDir: "/w", PlannerPrompt: "P"},
	}}
	r := NewKeyResolver(defaults, data)

	const goroutines, iters = 10, 100
	// sync.WaitGroup (not a done-channel) so the "all goroutines finish
	// before the test function returns" invariant is mechanically
	// enforced — a future edit that reorders assertions and the signal
	// would otherwise leave t.Errorf calls running after the test
	// returned, which panics. SS1 hardening.
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_, opts := r.ResolveForChat("feishu", "direct", "alice", "general")
				if len(opts.ExtraArgs) != 3 {
					t.Errorf("ExtraArgs len = %d, want 3", len(opts.ExtraArgs))
				}
			}
		}()
	}
	wg.Wait()

	// Defaults backing array must still look like before — len unchanged,
	// and capacity slots past len must be zero strings (append never
	// wrote into them).
	orig := defaults["general"].ExtraArgs
	if len(orig) != 1 {
		t.Errorf("defaults len changed from 1 to %d", len(orig))
	}
	peek := orig[:cap(orig)]
	for i := 1; i < cap(orig); i++ {
		if peek[i] != "" {
			t.Errorf("defaults backing array cell [%d] = %q, want empty — aliasing leaked", i, peek[i])
		}
	}
}

// ----- Historical key compatibility --------------------------------------

func TestResolveForKey_HistoricalKeys(t *testing.T) {
	t.Parallel()
	// Covers historical key shapes that have ever appeared in
	// sessions.json / WS subscribe. Resolver MUST NOT panic on any of
	// them; ok must match expectations.
	r := NewKeyResolver(map[string]AgentOpts{"general": {}}, nil)

	cases := []struct {
		key    string
		wantOK bool
	}{
		// standard IM 4-segment
		{"feishu:direct:alice:general", true},
		{"feishu:group:xxx:code-reviewer", true},
		{"dashboard:session:local:general", true},
		// scratch (reserved)
		{"scratch:abc123:general:general", false},
		// cron (reserved)
		{"cron:daily-standup", false},
		// planner (reserved; data=nil so always ok=false)
		{"project:naozhi:planner", false},
		// malformed — must not panic
		{"foo:bar", false},
		{"", false},
		{"a", false},
	}
	for _, tc := range cases {
		// Defensive: wrap each call so a panic surfaces as a clear failure.
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ResolveForKey(%q) panicked: %v", tc.key, r)
				}
			}()
			_, ok := r.ResolveForKey(tc.key)
			if ok != tc.wantOK {
				t.Errorf("ResolveForKey(%q) ok = %v, want %v", tc.key, ok, tc.wantOK)
			}
		}()
	}
}
