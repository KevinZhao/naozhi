package main

import (
	"errors"
	"strings"
	"testing"
)

func approved(issue int) ([]string, error) {
	switch issue {
	case 1:
		return []string{"source:x", approvedLabel}, nil
	case 2:
		return []string{"source:x"}, nil
	}
	return nil, errors.New("not found")
}

func TestCheckLedger(t *testing.T) {
	t.Parallel()
	r := []raise{{Gate: "go:a_test.go#fooBaseline", From: 5, To: 6}}
	const old = `{"gate":"old","from":1,"to":2,"issue":1,"reason":"r"}`
	const good = `{"gate":"go:a_test.go#fooBaseline","from":5,"to":6,"issue":1,"reason":"a bug fix needs one more"}`
	cases := []struct {
		name       string
		rs         []raise
		base, head string
		labels     labelSource
		want       string // substring of the single problem, "" for none
	}{
		{"approved raise", r, old, old + "\n" + good, approved, ""},
		{"no raise, no entry", nil, old, old, approved, ""},
		{"raise without an entry", r, old, old, approved, "with no ledger entry"},
		{"entry already in base does not count", r, old + "\n" + good, old + "\n" + good, approved, "with no ledger entry"},
		{"numbers disagree", r, "", strings.Replace(good, `"to":6`, `"to":7`, 1), approved, "says 5 -> 7"},
		{"issue not approved", r, "", strings.Replace(good, `"issue":1`, `"issue":2`, 1), approved, "lacks the"},
		{"issue lookup fails", r, "", strings.Replace(good, `"issue":1`, `"issue":3`, 1), approved, "not found"},
		{"no reason", r, "", strings.Replace(good, `"a bug fix needs one more"`, `" "`, 1), approved, "gives no reason"},
		{"entry with no raise", nil, "", good, approved, "matches no raise"},
		{"edited history", nil, old, strings.Replace(old, `"reason":"r"`, `"reason":"s"`, 1), approved, "append-only"},
		{"dropped history", nil, old, "", approved, "append-only"},
		{"no token: numbers still checked", r, "", good, nil, ""},
	}
	for _, tc := range cases {
		got := checkLedger(tc.rs, tc.base, tc.head, tc.labels)
		switch {
		case tc.want == "" && len(got) != 0:
			t.Errorf("%s: problems %q, want none", tc.name, got)
		case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
			t.Errorf("%s: problems %q, want one containing %q", tc.name, got, tc.want)
		}
	}
}
