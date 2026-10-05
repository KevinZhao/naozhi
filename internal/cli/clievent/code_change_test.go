package clievent

import (
	"strings"
	"testing"
)

var prA = CodeChange{Provider: "github", URL: "https://github.com/o/r/pull/12", Repo: "o/r", Identifier: "12", Action: "created", Branch: "feat-x"}

func TestCodeChangeValid(t *testing.T) {
	t.Parallel()
	ok := []CodeChange{
		prA,
		{URL: "http://ghe.corp/o/r/pull/3"},
		{Provider: "gerrit", URL: "https://x-review.googlesource.com/c/p/+/123", Identifier: "123", Action: "auto-merge-enabled"},
	}
	for _, c := range ok {
		if !c.Valid() {
			t.Errorf("Valid(%+v) = false, want true", c)
		}
	}
	bad := map[string]CodeChange{
		"empty url":       {},
		"javascript url":  {URL: "javascript:alert(1)//github.com/o/r/pull/1"},
		"data url":        {URL: "data:text/html,x"},
		"no host":         {URL: "https:///o/r/pull/1"},
		"credentials":     {URL: "https://u:p@github.com/o/r/pull/1"},
		"newline in url":  {URL: "https://github.com/o/r/pull/1\nx"},
		"newline in repo": {URL: prA.URL, Repo: "o/r\n"},
		"spaced action":   {URL: prA.URL, Action: "merged now"},
		"markup provider": {URL: prA.URL, Provider: "<b>"},
		"long branch":     {URL: prA.URL, Branch: strings.Repeat("b", 256)},
		"long url":        {URL: "https://github.com/" + strings.Repeat("a", 2048)},
	}
	for name, c := range bad {
		if c.Valid() {
			t.Errorf("%s: Valid(%+v) = true, want false", name, c)
		}
	}
}

func TestMergeCodeChange(t *testing.T) {
	t.Parallel()
	prB := CodeChange{Provider: "github", URL: "https://github.com/o/r/pull/13", Repo: "o/r", Identifier: "13", Action: "created"}

	list, changed := MergeCodeChange(nil, prA)
	if !changed || len(list) != 1 || list[0] != prA {
		t.Fatalf("first merge = %+v changed=%v", list, changed)
	}
	// The CLI re-announces the PR on every push to its branch.
	if again, changed := MergeCodeChange(list, prA); changed || len(again) != 1 {
		t.Errorf("repeat of the newest entry: changed=%v len=%d, want unchanged", changed, len(again))
	}

	list, _ = MergeCodeChange(list, prB)
	// A later action on A moves it to the end and keeps the branch only the
	// create carried.
	merged := CodeChange{Provider: "github", URL: prA.URL, Repo: "o/r", Identifier: "12", Action: "merged"}
	before := list
	list, changed = MergeCodeChange(list, merged)
	if !changed || len(list) != 2 || list[0].URL != prB.URL || list[1].Action != "merged" || list[1].Branch != "feat-x" {
		t.Fatalf("after merge action: %+v changed=%v", list, changed)
	}
	if before[0] != prA {
		t.Errorf("input list mutated: %+v", before)
	}
	// A pushed frame for an already-newest URL with nothing new is a no-op
	// even when it omits fields.
	if _, changed := MergeCodeChange(list, CodeChange{URL: prA.URL}); changed {
		t.Error("field-less repeat of the newest URL reported a change")
	}

	for i := range MaxCodeChanges + 3 {
		list, _ = MergeCodeChange(list, CodeChange{URL: "https://github.com/o/r/pull/" + strings.Repeat("9", i+1)})
	}
	if len(list) != MaxCodeChanges || list[len(list)-1].URL != "https://github.com/o/r/pull/"+strings.Repeat("9", MaxCodeChanges+3) {
		t.Errorf("cap: len=%d newest=%q, want %d keeping the newest", len(list), list[len(list)-1].URL, MaxCodeChanges)
	}
}
