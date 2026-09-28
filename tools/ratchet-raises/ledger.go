package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// approvedLabel is the label the owner puts on an issue to approve the raises
// that cite it.
const approvedLabel = "ratchet-raise-approved"

// entry is one ledger line: a raise, the issue that approves it, and why.
type entry struct {
	Gate   string `json:"gate"`
	From   int64  `json:"from"`
	To     int64  `json:"to"`
	Issue  int    `json:"issue"`
	Reason string `json:"reason"`
}

// labelSource answers which labels an issue carries.
type labelSource func(issue int) ([]string, error)

// checkLedger reports every problem with the raises between base and head:
// a ledger that is not an append of the base ledger, a raise with no entry
// added in this change, an added entry with no raise, and an entry whose issue
// is not approved. labels is nil when approval cannot be checked (no token);
// the other checks still run.
func checkLedger(rs []raise, baseLedger, headLedger string, labels labelSource) []string {
	var problems []string
	baseLines, headLines := ledgerLines(baseLedger), ledgerLines(headLedger)
	if len(headLines) < len(baseLines) {
		return []string{"scripts/ratchet-raises.jsonl lost lines; the ledger is append-only"}
	}
	for i, l := range baseLines {
		if headLines[i] != l {
			return []string{fmt.Sprintf("scripts/ratchet-raises.jsonl line %d was changed; the ledger is append-only", i+1)}
		}
	}

	added := map[string]entry{}
	for i, l := range headLines[len(baseLines):] {
		var e entry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			problems = append(problems, fmt.Sprintf("ledger line %d: %v", len(baseLines)+i+1, err))
			continue
		}
		switch {
		case e.Issue <= 0:
			problems = append(problems, fmt.Sprintf("ledger entry for %s names no issue", e.Gate))
		case strings.TrimSpace(e.Reason) == "":
			problems = append(problems, fmt.Sprintf("ledger entry for %s gives no reason", e.Gate))
		}
		added[e.Gate] = e
	}

	for _, r := range rs {
		e, ok := added[r.Gate]
		if !ok {
			want, _ := json.Marshal(entry{Gate: r.Gate, From: r.From, To: r.To, Issue: 0, Reason: "…"})
			problems = append(problems, fmt.Sprintf("%s rose %d -> %d with no ledger entry; append to scripts/ratchet-raises.jsonl:\n  %s", r.Gate, r.From, r.To, want))
			continue
		}
		delete(added, r.Gate)
		if e.From != r.From || e.To != r.To {
			problems = append(problems, fmt.Sprintf("ledger entry for %s says %d -> %d, the change is %d -> %d", r.Gate, e.From, e.To, r.From, r.To))
			continue
		}
		if labels == nil || e.Issue <= 0 {
			continue
		}
		got, err := labels(e.Issue)
		if err != nil {
			problems = append(problems, fmt.Sprintf("issue #%d for %s: %v", e.Issue, r.Gate, err))
			continue
		}
		if !contains(got, approvedLabel) {
			problems = append(problems, fmt.Sprintf("issue #%d for %s lacks the %q label", e.Issue, r.Gate, approvedLabel))
		}
	}
	for gate := range added {
		problems = append(problems, fmt.Sprintf("ledger entry for %s matches no raise in this change", gate))
	}
	return problems
}

func ledgerLines(raw string) []string {
	var out []string
	for _, l := range strings.Split(raw, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
