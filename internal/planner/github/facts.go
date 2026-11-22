package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/naozhi/naozhi/internal/planner/front"
)

// Ledger is the ledger's open items as the hourly refresh settles them
// (front.Store).
type Ledger interface {
	Items() []front.Item
	CloseItem(id, by string) (front.Item, error)
}

// factsMax is how many open items one refresh looks up: the rest wait for
// the next hour, which starts after the last one looked up.
const factsMax = 50

// fact is an open item whose source names something on GitHub.
type fact struct {
	item            front.Item
	kind, repo, dir string
	n               int
}

// settle closes the open items whose source is over (§15.23, D143): a pull
// request merged or closed, an issue closed. An item is closed by the fact,
// not by someone remembering to tick it. It rides on the hourly refresh,
// the one loop that already runs gh with no model. A source written "#n" is
// an issue of the repository the item's workspace is in (repos); with no
// such repository it is left to the Planner or the user. A lookup that
// fails closes nothing: not knowing is not merged.
func (w *Writer) settle(ctx context.Context, repos []Repo) {
	if w.cfg.Ledger == nil {
		return
	}
	var facts []fact
	for _, it := range w.cfg.Ledger.Items() {
		if kind, repo, dir, n, ok := factOf(it, repos); ok {
			facts = append(facts, fact{it, kind, repo, dir, n})
		}
	}
	// More than a round looks up: go on after the last one looked up, so
	// the oldest staying open do not keep the rest from ever being seen.
	if len(facts) > factsMax {
		from := slices.IndexFunc(facts, func(f fact) bool { return f.item.ID == w.settled }) + 1
		facts = append(facts[from:], facts[:from]...)[:factsMax]
		w.settled = facts[len(facts)-1].item.ID
	} else {
		w.settled = ""
	}
	// state is each source's answer this round: several items may name one
	// pull request.
	state := map[string]string{}
	closed := 0
	for _, f := range facts {
		if ctx.Err() != nil {
			return
		}
		key := strings.ToLower(fmt.Sprintf("%s %s#%d", f.kind, f.repo, f.n))
		st, seen := state[key]
		if !seen {
			var err error
			if st, err = w.state(ctx, f.dir, f.kind, f.repo, f.n); err != nil {
				slog.Debug("planner github: an item's source not looked up", "item", f.item.ID, "source", f.item.Source, "err", err)
			}
			state[key] = st
		}
		if st != "MERGED" && st != "CLOSED" {
			continue
		}
		if _, err := w.cfg.Ledger.CloseItem(f.item.ID, front.ByFact); err != nil {
			slog.Warn("planner github: an item whose source is over not closed", "item", f.item.ID, "err", err)
			continue
		}
		closed++
	}
	if len(facts) > 0 {
		slog.Info("planner github: open items settled", "looked", len(facts), "closed", closed)
	}
}

// factOf is what it's source names on GitHub: kind "pr" or "issue", the
// repository, a directory to run gh in ("" for none needed) and the number.
func factOf(it front.Item, repos []Repo) (kind, repo, dir string, n int, ok bool) {
	if owner, name, n, ok := front.SourcePR(it.Source); ok {
		return "pr", owner + "/" + name, "", n, true
	}
	owner, name, n, ok := front.SourceIssue(it.Source)
	switch {
	case !ok:
		return "", "", "", 0, false
	case owner != "":
		return "issue", owner + "/" + name, "", n, true
	case it.Workspace == "":
		return "", "", "", 0, false
	}
	i := slices.IndexFunc(repos, func(r Repo) bool { return slices.Contains(r.WorkspaceIDs, it.Workspace) })
	if i < 0 {
		return "", "", "", 0, false
	}
	return "issue", repos[i].Owner + "/" + repos[i].Name, repos[i].Dir, n, true
}

// state is the state gh reports of pull request or issue n of repo: OPEN,
// CLOSED or, for a pull request, MERGED.
func (w *Writer) state(ctx context.Context, dir, kind, repo string, n int) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	out, err := w.cfg.Exec(cctx, dir, "gh", kind, "view", fmt.Sprint(n), "-R", repo, "--json", "state")
	if err != nil {
		return "", err
	}
	var v struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("gh %s view: 输出不是 JSON：%w", kind, err)
	}
	return strings.ToUpper(v.State), nil
}
