package patrol

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/planner/front"
)

const (
	// The lists one repository is asked for are bounded.
	pullLimit  = 100
	runLimit   = 20
	issueLimit = 100
	// maxHeadLookups caps the branches a round asks a repository about one
	// by one; the rest wait for the next round.
	maxHeadLookups = 10
	titleRunes     = 200
)

// repoReport is what the checks found in one GitHub repository this round.
type repoReport struct {
	Owner, Name string
	// key is the lowercased owner/name the repository is told by.
	key string
	// full is owner/name as GitHub spells it, what the links and the
	// section are written with: two directories may spell their remote in
	// different cases, and a source that changed with the directory asked
	// would file the item again. The remote's own spelling until gh answers.
	full string
	// err is a gh call failing: the repository's section keeps its last
	// body and its items stay as they are.
	err error
	// def is the default branch.
	def string
	// parent is the owner/name of the repository this one is a fork of, ""
	// for none: where the pull requests of a fork's branches are.
	parent string
	// heads are the head branches of the open pull requests.
	heads map[string]bool
	pulls []hit
	ci    []hit
	stale []hit
	// lookups counts the branches asked about (maxHeadLookups).
	lookups int
}

// github is the round's report of the repository owner/name, asked through
// gh in dir by the first directory in it; its items are filed under that
// directory's workspace ws, and belong to the repository: another workspace
// in it finds them already there.
func (p *Patrol) github(ctx context.Context, rd *round, ws, owner, name, dir string) *repoReport {
	key := strings.ToLower(owner + "/" + name)
	if r, ok := rd.repos[key]; ok {
		return r
	}
	r := &repoReport{Owner: owner, Name: name, key: key, full: owner + "/" + name}
	rd.repos[key] = r
	r.err = p.ask(ctx, r, dir, rd.now)
	if r.err != nil {
		rd.failed++
		return r
	}
	for _, c := range []struct {
		name string
		hits []hit
	}{{checkPR, r.pulls}, {checkCI, r.ci}, {checkIssues, r.stale}} {
		p.reconcile(rd, ws, c.hits, func(it front.Item) bool {
			g, repo := groupOf(it.Source)
			return it.From == front.FromPatrol && g == c.name && repo == key
		})
	}
	return r
}

// pull is an open pull request as gh lists it.
type pull struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	CreatedAt   time.Time `json:"createdAt"`
	IsDraft     bool      `json:"isDraft"`
	HeadRefName string    `json:"headRefName"`
	Checks      []check   `json:"statusCheckRollup"`
}

// check is one entry of a pull request's statusCheckRollup: a check run
// (Status, Conclusion) or a commit status (State).
type check struct {
	Typename     string    `json:"__typename"`
	Name         string    `json:"name"`
	WorkflowName string    `json:"workflowName"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	Context      string    `json:"context"`
	State        string    `json:"state"`
	StartedAt    time.Time `json:"startedAt"`
}

// workflowRun is one run of a workflow a push to the default branch started.
type workflowRun struct {
	WorkflowName string    `json:"workflowName"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	CreatedAt    time.Time `json:"createdAt"`
	URL          string    `json:"url"`
}

// ask makes r's four gh calls; any failing fails the repository's checks
// together. Every argument is a part parsed out of the remote's URL or a
// value gh gave, never text as written elsewhere.
func (p *Patrol) ask(ctx context.Context, r *repoReport, dir string, now time.Time) error {
	repo := r.Owner + "/" + r.Name
	var view struct {
		NameWithOwner    string `json:"nameWithOwner"`
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
		Parent *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"parent"`
	}
	if err := p.gh(ctx, dir, &view, "repo", "view", "--json", "defaultBranchRef,nameWithOwner,parent", "--", repo); err != nil {
		return err
	}
	r.def = view.DefaultBranchRef.Name
	if v := view.Parent; v != nil && repoRE.MatchString(v.Owner.Login+"/"+v.Name) {
		r.parent = v.Owner.Login + "/" + v.Name
	}
	// Only another casing of the same name is taken: a repository renamed
	// on GitHub answers with its new name, which the items' sources would
	// then no longer be told by.
	if strings.EqualFold(view.NameWithOwner, r.key) {
		r.full = view.NameWithOwner
	}

	// Open ones only: among the latest hundred of every state, a busy
	// repository's pull request open for two weeks is long gone.
	var pulls []pull
	if err := p.gh(ctx, dir, &pulls, "pr", "list", "-R", repo, "--state", "open", "--limit", fmt.Sprint(pullLimit),
		"--json", "number,title,createdAt,isDraft,headRefName,statusCheckRollup"); err != nil {
		return err
	}
	r.heads = map[string]bool{}
	for _, pr := range pulls {
		r.heads[pr.HeadRefName] = true
	}
	r.pulls = stalled(r, pulls, now)

	// A repository with no commit has no default branch, and no CI.
	if r.def != "" {
		var runs []workflowRun
		// Pushes only: --branch is a run's head branch, which a fork's pull
		// request from its own branch of that name has too, as has every
		// workflow an issue, a comment or a schedule starts; those are not
		// the branch's CI, and would crowd it out of the list.
		if err := p.gh(ctx, dir, &runs, "run", "list", "-R", repo, "--branch="+r.def, "--event", "push", "--limit", fmt.Sprint(runLimit),
			"--json", "workflowName,status,conclusion,createdAt,url"); err != nil {
			return err
		}
		r.ci = failing(r, runs, now.Location())
	}

	since := now.AddDate(0, 0, -issueDays)
	var issues []struct {
		UpdatedAt time.Time `json:"updatedAt"`
	}
	if err := p.gh(ctx, dir, &issues, "issue", "list", "-R", repo, "--state", "open",
		"--search", "updated:<"+since.UTC().Format(time.DateOnly), "--limit", fmt.Sprint(issueLimit), "--json", "number,updatedAt"); err != nil {
		return err
	}
	// The search counts whole days in UTC; the check says 30 days.
	n := 0
	for _, i := range issues {
		if i.UpdatedAt.Before(since) {
			n++
		}
	}
	if n > 0 {
		count := fmt.Sprint(n)
		if len(issues) >= issueLimit {
			count = fmt.Sprintf("%d 个以上", issueLimit)
		} else {
			count += " 个"
		}
		text := fmt.Sprintf("%s issue 超过 %d 天没动", count, issueDays)
		link := "https://github.com/" + r.full + "/issues?q=is%3Aissue+is%3Aopen+sort%3Aupdated-asc"
		r.stale = []hit{{Source: link, Text: text, Line: text + " · " + link}}
	}
	return nil
}

// gh runs gh with args in dir and decodes its JSON output into v.
func (p *Patrol) gh(ctx context.Context, dir string, v any, args ...string) error {
	out, err := p.call(ctx, dir, "gh", args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("gh %s %s: 输出不是 JSON：%w", args[0], args[1], err)
	}
	return nil
}

// stalled is the pull requests open prDays or more, not drafts, whose CI
// passed, the oldest first. The item's source is the pull request's link,
// built from the repository and the number: the user can open it, and the
// hourly GitHub refresh closes the item once the pull request is merged or
// closed even on a round the patrol did not make.
func stalled(r *repoReport, pulls []pull, now time.Time) []hit {
	pulls = slices.Clone(pulls)
	slices.SortFunc(pulls, func(a, b pull) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), a.Number-b.Number)
	})
	var hits []hit
	for _, pr := range pulls {
		if pr.Number <= 0 || pr.IsDraft || now.Sub(pr.CreatedAt) < prDays*day || !green(pr.Checks) {
			continue
		}
		link := fmt.Sprintf("https://github.com/%s/pull/%d", r.full, pr.Number)
		lead := fmt.Sprintf("PR #%d 开了 %d 天、CI 通过，还没合：", pr.Number, days(now, pr.CreatedAt))
		title := fold(pr.Title)
		hits = append(hits, hit{Source: link, Text: lead + title, Line: lead + clip(title, titleRunes) + " · " + link})
	}
	return hits
}

// green reports whether a pull request's checks passed: it has some, and
// the latest run of each is a success (or neutral, or skipped). One with no
// check at all is not "CI 通过": nothing looked at it.
func green(checks []check) bool {
	latest := map[string]check{}
	for _, c := range checks {
		// A re-run check is listed once per run.
		key := c.Typename + "\x00" + c.WorkflowName + "\x00" + c.Name + "\x00" + c.Context
		if o, ok := latest[key]; !ok || !c.StartedAt.Before(o.StartedAt) {
			latest[key] = c
		}
	}
	for _, c := range latest {
		if c.Typename == "StatusContext" {
			if c.State != "SUCCESS" {
				return false
			}
			continue
		}
		if c.Status != "COMPLETED" || !slices.Contains([]string{"SUCCESS", "NEUTRAL", "SKIPPED"}, c.Conclusion) {
			return false
		}
	}
	return len(latest) > 0
}

// failing is one hit when the default branch's CI is failing: of each
// workflow the latest completed run among runs (those pushes to the branch
// started) is taken (a repository's "last CI run" is one per workflow), and
// any of them failed. The source is the Actions page of the branch, the
// same while it fails.
func failing(r *repoReport, runs []workflowRun, loc *time.Location) []hit {
	latest := map[string]workflowRun{}
	for _, run := range runs {
		if run.Status != "completed" {
			continue
		}
		if o, ok := latest[run.WorkflowName]; !ok || run.CreatedAt.After(o.CreatedAt) {
			latest[run.WorkflowName] = run
		}
	}
	var failed []workflowRun
	for _, run := range latest {
		if slices.Contains([]string{"failure", "timed_out", "startup_failure"}, run.Conclusion) {
			failed = append(failed, run)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	slices.SortFunc(failed, func(a, b workflowRun) int { return strings.Compare(a.WorkflowName, b.WorkflowName) })
	var names, lines []string
	for _, run := range failed {
		name := cmp.Or(fold(run.WorkflowName), "（未命名）")
		names = append(names, name)
		line := name + "（" + run.CreatedAt.In(loc).Format(timeLayout)
		if link := fold(run.URL); strings.HasPrefix(link, "https://github.com/") {
			line += " · " + link
		}
		lines = append(lines, line+"）")
	}
	def := fold(r.def)
	lead := def + " 最近一次 CI 失败："
	link := "https://github.com/" + r.full + "/actions?query=branch%3A" + url.QueryEscape(r.def)
	return []hit{{Source: link, Text: lead + strings.Join(names, "、"), Line: lead + strings.Join(lines, "、")}}
}

// hasPull reports whether branch (as origin names it) has, or had, a pull
// request in r; known is false when this round's lookups are spent. A
// branch found to have one is remembered: that does not change.
func (p *Patrol) hasPull(ctx context.Context, r *repoReport, dir, branch string) (has, known bool, err error) {
	key := r.key + "\x00" + branch
	if r.heads[branch] || p.hasPR[key] {
		return true, true, nil
	}
	if r.lookups >= maxHeadLookups {
		return false, false, nil
	}
	r.lookups++
	// A fork's pull requests are opened in the repository it was forked
	// from: `gh pr list -R` lists those into the repository named.
	repos := []string{r.Owner + "/" + r.Name}
	if r.parent != "" {
		repos = append(repos, r.parent)
	}
	for _, repo := range repos {
		var prs []struct {
			Number int `json:"number"`
		}
		// --head= in one argument: a branch name is never read as a flag.
		if err := p.gh(ctx, dir, &prs, "pr", "list", "-R", repo, "--head="+branch, "--state", "all", "--limit", "1", "--json", "number"); err != nil {
			return false, false, err
		}
		if len(prs) > 0 {
			p.hasPR[key] = true
			return true, true, nil
		}
	}
	return false, true, nil
}
