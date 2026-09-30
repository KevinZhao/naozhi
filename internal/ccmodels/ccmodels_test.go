package ccmodels

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// realSnapshot mirrors the toolbox wrapper's own output closely enough that a
// regression in key handling shows up as a wrong alias list, not a parse error.
const realSnapshot = `{
  "awsCredentialExport": "\"/x/claude\" default-credential-export",
  "enforceAvailableModels": true,
  "model": "global.anthropic.claude-opus-4-8[1m]",
  "modelOverrides.claude-fable-5": "global.anthropic.claude-fable-5[1m]",
  "modelOverrides.claude-fable-5[1m]": "global.anthropic.claude-fable-5[1m]",
  "modelOverrides.claude-haiku-4-5": "global.anthropic.claude-haiku-4-5-20251001-v1:0",
  "modelOverrides.claude-opus-4-8": "global.anthropic.claude-opus-4-8",
  "modelOverrides.claude-opus-4-8[1m]": "global.anthropic.claude-opus-4-8[1m]",
  "modelOverrides.claude-opus-5": "global.anthropic.claude-opus-5",
  "modelOverrides.claude-sonnet-5": "global.anthropic.claude-sonnet-5",
  "outputStyle": "Concise"
}`

func TestParseSnapshot_TakesOverrideKeysOnly(t *testing.T) {
	snap, err := ParseSnapshot([]byte(realSnapshot))
	if err != nil {
		t.Fatalf("ParseSnapshot: %v", err)
	}
	if snap.Model != "global.anthropic.claude-opus-4-8[1m]" {
		t.Errorf("Model = %q", snap.Model)
	}
	got := make([]string, 0, len(snap.Aliases))
	for _, a := range snap.Aliases {
		got = append(got, a.Name)
	}
	want := []string{
		"claude-opus-5",
		"claude-opus-4-8[1m]", "claude-opus-4-8",
		"claude-sonnet-5",
		"claude-fable-5[1m]", "claude-fable-5",
		"claude-haiku-4-5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("aliases:\n got %v\nwant %v", got, want)
	}
}

func TestParseSnapshot_RejectsNonObject(t *testing.T) {
	if _, err := ParseSnapshot([]byte(`["a"]`)); err == nil {
		t.Fatal("want error for non-object snapshot")
	}
}

func TestSortAliases_IsStableAcrossInputOrder(t *testing.T) {
	snap, err := ParseSnapshot([]byte(realSnapshot))
	if err != nil {
		t.Fatalf("ParseSnapshot: %v", err)
	}
	// Map iteration already randomises input order; re-parse must not drift.
	for i := 0; i < 20; i++ {
		again, err := ParseSnapshot([]byte(realSnapshot))
		if err != nil {
			t.Fatalf("ParseSnapshot: %v", err)
		}
		if !reflect.DeepEqual(again.Aliases, snap.Aliases) {
			t.Fatalf("order drifted on run %d:\n got %v\nwant %v", i, again.Aliases, snap.Aliases)
		}
	}
}

func TestSortAliases_UnknownFamilySortsLastAlphabetically(t *testing.T) {
	as := []Alias{
		{Name: "claude-zephyr-1"},
		{Name: "claude-haiku-4-5"},
		{Name: "claude-aardvark-1"},
		{Name: "claude-opus-5"},
	}
	sortAliases(as)
	want := []string{"claude-opus-5", "claude-haiku-4-5", "claude-aardvark-1", "claude-zephyr-1"}
	for i, a := range as {
		if a.Name != want[i] {
			t.Fatalf("position %d = %q, want %q (full: %v)", i, a.Name, want[i], as)
		}
	}
}

func TestRepairCandidate(t *testing.T) {
	cases := []struct {
		name, alias, profile, want string
	}{
		{"dated profile repairs bare alias",
			"claude-haiku-4-5", "global.anthropic.claude-haiku-4-5-20251001-v1:0",
			"claude-haiku-4-5-20251001"},
		{"1m suffix is carried onto the repair",
			"claude-haiku-4-5[1m]", "global.anthropic.claude-haiku-4-5-20251001-v1:0[1m]",
			"claude-haiku-4-5-20251001[1m]"},
		{"matching spelling needs no repair",
			"claude-opus-5", "global.anthropic.claude-opus-5", ""},
		{"profile-only 1m is not a repair",
			"claude-fable-5", "global.anthropic.claude-fable-5[1m]", ""},
		{"non-claude profile yields nothing",
			"some-model", "global.amazon.nova-pro-v1:0", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RepairCandidate(Alias{Name: tc.alias, Profile: tc.profile})
			if tc.want == "" {
				if ok {
					t.Fatalf("want no repair, got %q", got.Name)
				}
				return
			}
			if !ok {
				t.Fatal("want a repair, got none")
			}
			if got.Name != tc.want {
				t.Errorf("Name = %q, want %q", got.Name, tc.want)
			}
			if got.Profile != tc.profile {
				t.Errorf("Profile = %q, want it unchanged", got.Profile)
			}
		})
	}
}

func TestCandidates_IncludesRepairsAndDedupes(t *testing.T) {
	snap := Snapshot{Aliases: []Alias{
		{Name: "claude-haiku-4-5", Profile: "global.anthropic.claude-haiku-4-5-20251001-v1:0"},
		{Name: "claude-haiku-4-5-20251001", Profile: "global.anthropic.claude-haiku-4-5-20251001-v1:0"},
		{Name: "claude-opus-5", Profile: "global.anthropic.claude-opus-5"},
	}}
	got := make([]string, 0, 3)
	for _, a := range Candidates(snap) {
		got = append(got, a.Name)
	}
	want := []string{"claude-haiku-4-5", "claude-haiku-4-5-20251001", "claude-opus-5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildPlan_UnprobedAliasesAreKept(t *testing.T) {
	snap, _ := ParseSnapshot([]byte(realSnapshot))
	plan := BuildPlan(snap, nil)
	if len(plan.Excluded) != 0 {
		t.Errorf("Excluded = %v, want none when nothing was probed", plan.Excluded)
	}
	if len(plan.Aliases) != len(snap.Aliases) {
		t.Errorf("kept %d of %d aliases", len(plan.Aliases), len(snap.Aliases))
	}
}

func TestBuildPlan_ExcludesOnlyDecidedFailures(t *testing.T) {
	snap := Snapshot{Aliases: []Alias{
		{Name: "claude-opus-5", Profile: "global.anthropic.claude-opus-5"},
		{Name: "claude-fable-5-1", Profile: "global.anthropic.claude-fable-5-1"},
		{Name: "claude-sonnet-5", Profile: "global.anthropic.claude-sonnet-5"},
	}}
	plan := BuildPlan(snap, map[string]Verdict{
		"claude-opus-5":    {Status: StatusOK},
		"claude-fable-5-1": {Status: StatusDenied, Detail: "AccessDeniedException"},
		"claude-sonnet-5":  {Status: StatusUnknown, Detail: "throttled"},
	})
	if got := plan.Available(); !reflect.DeepEqual(got, []string{"claude-opus-5", "claude-sonnet-5"}) {
		t.Errorf("Available = %v", got)
	}
	if len(plan.Excluded) != 1 || plan.Excluded[0].Alias != "claude-fable-5-1" {
		t.Fatalf("Excluded = %v", plan.Excluded)
	}
	if plan.Excluded[0].Detail != "AccessDeniedException" {
		t.Errorf("lost the deciding detail: %+v", plan.Excluded[0])
	}
}

func TestBuildPlan_PrefersRepairSpelling(t *testing.T) {
	snap := Snapshot{Aliases: []Alias{
		{Name: "claude-haiku-4-5", Profile: "global.anthropic.claude-haiku-4-5-20251001-v1:0"},
	}}
	plan := BuildPlan(snap, map[string]Verdict{
		"claude-haiku-4-5":          {Status: StatusBadAlias},
		"claude-haiku-4-5-20251001": {Status: StatusOK},
	})
	if got := plan.Available(); !reflect.DeepEqual(got, []string{"claude-haiku-4-5-20251001"}) {
		t.Fatalf("Available = %v", got)
	}
	if len(plan.Repaired) != 1 || plan.Repaired[0].From != "claude-haiku-4-5" {
		t.Errorf("Repaired = %+v", plan.Repaired)
	}
}

func TestBuildPlan_FallsBackToSnapshotSpellingWhenRepairFails(t *testing.T) {
	snap := Snapshot{Aliases: []Alias{
		{Name: "claude-haiku-4-5", Profile: "global.anthropic.claude-haiku-4-5-20251001-v1:0"},
	}}
	plan := BuildPlan(snap, map[string]Verdict{
		"claude-haiku-4-5":          {Status: StatusOK},
		"claude-haiku-4-5-20251001": {Status: StatusDenied},
	})
	if got := plan.Available(); !reflect.DeepEqual(got, []string{"claude-haiku-4-5"}) {
		t.Fatalf("Available = %v", got)
	}
	if len(plan.Repaired) != 0 {
		t.Errorf("Repaired = %+v, want none", plan.Repaired)
	}
}

func TestPlan_EmptyGuardsAnEmptyAllowlist(t *testing.T) {
	if !(Plan{}).Empty() {
		t.Error("zero Plan must report Empty")
	}
	if (Plan{Aliases: []Alias{{Name: "x"}}}).Empty() {
		t.Error("non-empty Plan must not report Empty")
	}
}

func TestPatchSettings_PreservesOtherKeysAndOrder(t *testing.T) {
	doc := []byte(`{
  "env": {"A": "1"},
  "availableModels": ["stale"],
  "outputStyle": "Concise"
}`)
	plan := Plan{Aliases: []Alias{
		{Name: "claude-opus-5", Profile: "global.anthropic.claude-opus-5"},
	}}
	out, err := PatchSettings(doc, plan)
	if err != nil {
		t.Fatalf("PatchSettings: %v", err)
	}
	keys := topLevelKeyOrder(t, out)
	want := []string{"env", "availableModels", "outputStyle", "modelOverrides", "modelPicker"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("key order = %v, want %v", keys, want)
	}
	var got struct {
		Available []string          `json:"availableModels"`
		Overrides map[string]string `json:"modelOverrides"`
		Style     string            `json:"outputStyle"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(got.Available, []string{"claude-opus-5"}) {
		t.Errorf("availableModels = %v", got.Available)
	}
	if got.Overrides["claude-opus-5"] != "global.anthropic.claude-opus-5" {
		t.Errorf("modelOverrides = %v", got.Overrides)
	}
	if got.Style != "Concise" {
		t.Errorf("untouched key lost: outputStyle = %q", got.Style)
	}
}

func TestPatchSettings_IsIdempotent(t *testing.T) {
	plan := Plan{Aliases: []Alias{
		{Name: "claude-opus-5[1m]", Profile: "global.anthropic.claude-opus-5[1m]"},
		{Name: "claude-opus-5", Profile: "global.anthropic.claude-opus-5"},
	}}
	once, err := PatchSettings([]byte(`{}`), plan)
	if err != nil {
		t.Fatalf("PatchSettings: %v", err)
	}
	twice, err := PatchSettings(once, plan)
	if err != nil {
		t.Fatalf("PatchSettings: %v", err)
	}
	if string(once) != string(twice) {
		t.Errorf("second patch changed the document:\n%s\n---\n%s", once, twice)
	}
	d, err := DiffSettings(once, plan)
	if err != nil {
		t.Fatalf("DiffSettings: %v", err)
	}
	if !d.Empty() {
		t.Errorf("diff against own output = %+v, want empty", d)
	}
}

func TestDiffSettings(t *testing.T) {
	doc := []byte(`{
  "availableModels": ["claude-opus-5", "claude-haiku-4-5", "claude-gone-1"],
  "modelOverrides": {
    "claude-opus-5": "global.anthropic.claude-opus-5",
    "claude-haiku-4-5": "global.anthropic.claude-haiku-4-5",
    "claude-gone-1": "global.anthropic.claude-gone-1"
  }
}`)
	plan := Plan{Aliases: []Alias{
		{Name: "claude-opus-5", Profile: "global.anthropic.claude-opus-5"},
		{Name: "claude-haiku-4-5", Profile: "global.anthropic.claude-haiku-4-5-20251001-v1:0"},
		{Name: "claude-sonnet-5", Profile: "global.anthropic.claude-sonnet-5"},
	}}
	d, err := DiffSettings(doc, plan)
	if err != nil {
		t.Fatalf("DiffSettings: %v", err)
	}
	if !reflect.DeepEqual(d.Added, []string{"claude-sonnet-5"}) {
		t.Errorf("Added = %v", d.Added)
	}
	if !reflect.DeepEqual(d.Removed, []string{"claude-gone-1"}) {
		t.Errorf("Removed = %v", d.Removed)
	}
	if len(d.Remapped) != 1 || d.Remapped[0].Alias != "claude-haiku-4-5" ||
		d.Remapped[0].To != "global.anthropic.claude-haiku-4-5-20251001-v1:0" {
		t.Errorf("Remapped = %+v", d.Remapped)
	}
}

func TestDiffSettings_FreshDocumentIsAllAdded(t *testing.T) {
	plan := Plan{Aliases: []Alias{{Name: "claude-opus-5", Profile: "p"}}}
	for _, doc := range []string{"", "{}", `{"outputStyle":"Concise"}`} {
		d, err := DiffSettings([]byte(doc), plan)
		if err != nil {
			t.Fatalf("DiffSettings(%q): %v", doc, err)
		}
		if !reflect.DeepEqual(d.Added, []string{"claude-opus-5"}) || len(d.Removed) != 0 {
			t.Errorf("DiffSettings(%q) = %+v", doc, d)
		}
	}
}

func TestDiffSettings_RejectsNonObject(t *testing.T) {
	if _, err := DiffSettings([]byte(`[1]`), Plan{}); err == nil {
		t.Fatal("want error for non-object settings document")
	}
}

// topLevelKeyOrder returns the document's keys as written, which is what makes
// PatchSettings safe to run against a hand-edited file.
func topLevelKeyOrder(t *testing.T, doc []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(doc)))
	if _, err := dec.Token(); err != nil {
		t.Fatalf("read opening token: %v", err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		keys = append(keys, tok.(string))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			t.Fatalf("read value: %v", err)
		}
	}
	return keys
}

// TestPickerRows_OfferEveryAliasWithAVouch is what makes the interactive /model
// list equal the reconciled list: cc's own lineup is one row per family, so every
// older version and every [1m] variant needs a row of its own, and any spelling
// cc's catalog lacks needs behavesAs or the row is dropped.
func TestPickerRows_OfferEveryAliasWithAVouch(t *testing.T) {
	plan := Plan{
		Aliases: []Alias{
			{Name: "claude-opus-5[1m]", Profile: "global.anthropic.claude-opus-5[1m]"},
			{Name: "claude-opus-5", Profile: "global.anthropic.claude-opus-5"},
			{Name: "claude-sonnet-4-6", Profile: "global.anthropic.claude-sonnet-4-6"},
			{Name: "claude-haiku-4-5-20251001", Profile: "global.anthropic.claude-haiku-4-5-20251001-v1:0"},
		},
		Windows: map[string]int{"claude-opus-5[1m]": 1_000_000, "claude-opus-5": 200_000},
	}
	rows := plan.PickerRows()
	if len(rows) != len(plan.Aliases) {
		t.Fatalf("got %d rows for %d aliases", len(rows), len(plan.Aliases))
	}
	for i, a := range plan.Aliases {
		if rows[i].Model != a.Name {
			t.Errorf("row %d model = %q, want %q (order must match the plan)", i, rows[i].Model, a.Name)
		}
		if rows[i].Label == "" {
			t.Errorf("row %d has no label", i)
		}
	}
	want := map[string]struct{ label, desc, behavesAs string }{
		"claude-opus-5[1m]":         {"Opus 5 · 1M", "claude-opus-5[1m] · 1M context", "claude-opus-5"},
		"claude-opus-5":             {"Opus 5", "claude-opus-5 · 200k context", ""},
		"claude-sonnet-4-6":         {"Sonnet 4.6", "claude-sonnet-4-6", ""},
		"claude-haiku-4-5-20251001": {"Haiku 4.5", "claude-haiku-4-5-20251001", "claude-haiku-4-5"},
	}
	for _, r := range rows {
		w, ok := want[r.Model]
		if !ok {
			t.Errorf("unexpected row %q", r.Model)
			continue
		}
		if r.Label != w.label || r.Description != w.desc || r.BehavesAs != w.behavesAs {
			t.Errorf("row %q = {%q, %q, behavesAs %q}, want {%q, %q, behavesAs %q}",
				r.Model, r.Label, r.Description, r.BehavesAs, w.label, w.desc, w.behavesAs)
		}
	}
}

// TestPatchSettings_WritesPickerThatReplacesTheBuiltInLineup: without
// replaceBuiltInOptions cc keeps its per-family rows, which shadow ours for the
// families they cover.
func TestPatchSettings_WritesPickerThatReplacesTheBuiltInLineup(t *testing.T) {
	plan := Plan{Aliases: []Alias{
		{Name: "claude-opus-5[1m]", Profile: "global.anthropic.claude-opus-5[1m]"},
		{Name: "claude-fable-5", Profile: "global.anthropic.claude-fable-5[1m]"},
	}}
	doc, err := PatchSettings([]byte(`{"outputStyle":"Concise"}`), plan)
	if err != nil {
		t.Fatalf("PatchSettings: %v", err)
	}
	var got struct {
		OutputStyle string `json:"outputStyle"`
		Picker      struct {
			Options               []PickerRow `json:"options"`
			ReplaceBuiltInOptions bool        `json:"replaceBuiltInOptions"`
		} `json:"modelPicker"`
	}
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, doc)
	}
	if got.OutputStyle != "Concise" {
		t.Errorf("operator key lost: %q", got.OutputStyle)
	}
	if !got.Picker.ReplaceBuiltInOptions {
		t.Error("replaceBuiltInOptions false — cc's per-family lineup would shadow these rows")
	}
	if len(got.Picker.Options) != 2 || got.Picker.Options[0].Model != "claude-opus-5[1m]" {
		t.Errorf("options = %+v", got.Picker.Options)
	}
}

// TestDiffSettings_ReportsPickerDriftAlone: a label or window change must not be
// silently skipped just because the offered set is unchanged.
func TestDiffSettings_ReportsPickerDriftAlone(t *testing.T) {
	plan := Plan{Aliases: []Alias{{Name: "claude-opus-5", Profile: "global.anthropic.claude-opus-5"}}}
	synced, err := PatchSettings(nil, plan)
	if err != nil {
		t.Fatalf("PatchSettings: %v", err)
	}
	d, err := DiffSettings(synced, plan)
	if err != nil {
		t.Fatalf("DiffSettings: %v", err)
	}
	if !d.Empty() {
		t.Errorf("re-syncing an unchanged document diffs as %+v, want empty", d)
	}

	withWindow := plan
	withWindow.Windows = map[string]int{"claude-opus-5": 200_000}
	d, err = DiffSettings(synced, withWindow)
	if err != nil {
		t.Fatalf("DiffSettings: %v", err)
	}
	if len(d.Added) != 0 || len(d.Removed) != 0 || len(d.Remapped) != 0 {
		t.Errorf("offered set changed: %+v", d)
	}
	if !d.PickerRows {
		t.Error("a newly measured window did not register as picker drift")
	}
}
