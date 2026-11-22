package patrol

import (
	"cmp"
	"log/slog"
	"slices"
	"time"
)

// Report is what the morning brief says of the patrol (§15.23 ⑤).
type Report struct {
	// PRs are the pull requests ready and waiting on the user.
	PRs []ReadyPR
	// New are the findings first found in the brief's window that are
	// still to be settled.
	New []NewFinding
	// Stopped are the kinds the gateway stopped reporting that no brief
	// has said yet.
	Stopped []StoppedKind
}

// ReadyPR is a finding's pull request that waits on the user.
type ReadyPR struct {
	// Title is the finding's, URL the pull request's, CI its checks as
	// last looked at ("" before the first look), Workspace the workspace's
	// name.
	Title, URL, CI, Workspace string
	// Fresh says it became ready in the brief's window.
	Fresh bool
}

// NewFinding is a finding as the brief names it.
type NewFinding struct {
	Title, Kind, Workspace string
}

// StoppedKind is a kind no longer reported: Adopted of the last Of findings
// of it the user settled were taken.
type StoppedKind struct {
	Kind        string
	Adopted, Of int
}

// Brief is the patrol's part of a morning brief whose window starts at
// since, read off the records as they stand: the brief makes no gh call.
func (p *Patrol) Brief(since time.Time) Report {
	var rep Report
	if p == nil || p.cfg.Dir == "" {
		return rep
	}
	type found struct {
		NewFinding
		first time.Time
	}
	var fresh []found
	for _, st := range p.deepStates() {
		name := p.name(st.Workspace)
		for _, r := range st.Findings {
			switch {
			case r.State == StatePR:
				rep.PRs = append(rep.PRs, ReadyPR{Title: r.Title, URL: r.PR, CI: r.CI, Workspace: name, Fresh: !r.Ready.Before(since)})
			case !r.First.Before(since) && (unsettled(r.State) || r.State == StateDispatched) && !r.Pending:
				fresh = append(fresh, found{NewFinding{Title: r.Title, Kind: r.Kind, Workspace: name}, r.First})
			}
		}
	}
	slices.SortStableFunc(fresh, func(a, b found) int { return a.first.Compare(b.first) })
	for _, f := range fresh {
		rep.New = append(rep.New, f.NewFinding)
	}
	p.deepMu.Lock()
	d, err := p.loadDeclined()
	p.deepMu.Unlock()
	if err != nil {
		return rep
	}
	for _, k := range d.Kinds {
		if k.By == declinedByAuto && !k.Briefed {
			rep.Stopped = append(rep.Stopped, StoppedKind{Kind: k.Kind, Adopted: k.Adopted, Of: cmp.Or(k.Of, adoptionWindow)})
		}
	}
	return rep
}

// Briefed marks the stopped kinds a sent brief named, so each is said once.
func (p *Patrol) Briefed(kinds []string) {
	if p == nil || p.cfg.Dir == "" || len(kinds) == 0 {
		return
	}
	now := p.cfg.Now()
	p.deepMu.Lock()
	defer p.deepMu.Unlock()
	d, err := p.loadDeclined()
	if err != nil {
		return
	}
	changed := false
	for i := range d.Kinds {
		if k := &d.Kinds[i]; k.By == declinedByAuto && !k.Briefed && slices.Contains(kinds, k.Kind) {
			k.Briefed, changed = true, true
		}
	}
	if !changed {
		return
	}
	if err := p.saveDeclined(d, now); err != nil {
		slog.Warn("planner patrol: stopped kinds the brief named not marked; the next brief names them again", "err", err)
	}
}

// Detail is a finding as the page shows it under its open item.
type Detail struct {
	Kind       string     `json:"kind"`
	Size       string     `json:"size"`
	Confidence string     `json:"confidence"`
	Evidence   []Evidence `json:"evidence"`
	Impact     string     `json:"impact"`
	Fix        string     `json:"fix"`
	State      string     `json:"state"`
	PR         string     `json:"pr,omitempty"`
	CI         string     `json:"ci,omitempty"`
}

// Evidence is one piece of a finding's evidence: the text as reported, and
// where it opens ("" when it does not: a file line of a repository that is
// not on GitHub).
type Evidence struct {
	Text string `json:"text"`
	Link string `json:"link,omitempty"`
}

// Details are the findings refs name, by ref; one not on record is left
// out.
func (p *Patrol) Details(refs []string) map[string]Detail {
	out := map[string]Detail{}
	for _, ref := range refs {
		r, ok := p.Finding(ref)
		if !ok {
			continue
		}
		d := Detail{Kind: r.Kind, Size: r.Size, Confidence: r.Confidence, Impact: r.Impact, Fix: r.Fix, State: r.State, PR: r.PR, CI: r.CI,
			Evidence: make([]Evidence, 0, len(r.Evidence))}
		// The links are the file lines' in order (blobLinks).
		links := r.Links
		for _, e := range r.Evidence {
			ev := Evidence{Text: e}
			if _, _, _, file := fileLine(e); file && len(links) > 0 {
				ev.Link, links = links[0], links[1:]
			}
			d.Evidence = append(d.Evidence, ev)
		}
		out[ref] = d
	}
	return out
}
