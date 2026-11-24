package meeting

import (
	"bufio"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// GlossaryFile is the user's own term list in the meetings directory: one term per line, "#"
// starts a comment. Its terms come first.
const GlossaryFile = "glossary.txt"

// maxDocBytes caps what is read of one workspace document.
const maxDocBytes = 256 << 10

// glossaryDocs are the documents of a workspace read for its terms: the ones that say what the
// work is about, not the whole tree.
var glossaryDocs = []string{"CLAUDE.md", "README.md", "AGENTS.md", "memory.md"}

// termRE is a Latin token that reads as a name rather than a word: an acronym (GTM, EC2), a
// mixed-case name (JoyAI, OneDrive) or a code with digits (p5en, G7e).
var termRE = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+]{1,20}\b`)

func nameLike(w string) bool {
	upper, lower, digit, inner := 0, 0, 0, false
	for i, r := range w {
		switch {
		case r >= 'A' && r <= 'Z':
			upper++
			if i > 0 {
				inner = true
			}
		case r >= 'a' && r <= 'z':
			lower++
		case r >= '0' && r <= '9':
			digit++
		}
	}
	// "v1"-style version tags and plain capitalised words ("The") are not terms.
	if len(w) <= 2 && digit > 0 && upper == 0 {
		return false
	}
	return upper >= 2 || inner || (digit > 0 && lower+upper > 0)
}

var codeRE = regexp.MustCompile("`[^`\n]*`|https?://\\S+")

// commonTerms are name-like tokens every technical document has; telling recognition to expect
// them only crowds out the user's own.
var commonTerms = map[string]bool{}

func init() {
	for _, t := range strings.Fields(`README CLAUDE AGENTS LICENSE CHANGELOG TODO FAQ JSON YAML HTML CSS CSV PDF XML URL
		HTTP HTTPS API SDK CLI UI UX ID IP OS GB MB KB GiB MiB TB ALL AND NOT THE AMAZON CONFIDENTIAL
		NOTE TBD WIP OK PR CI CD`) {
		commonTerms[t] = true
	}
}

// recentDocs is how recently a workspace's documents must have changed for its terms to count:
// the user's current work, not every project on the disk.
const recentDocs = 30 * 24 * time.Hour

// Glossary is the terms recognition is told to expect (§3.5 热词): the user's list
// (root/glossary.txt), then, for the workspaces given (those the meeting names), the name-like tokens of the workspaces' documents, most telling
// first (frequent in a document, rare across workspaces), then the workspace names, max in all; only workspaces whose documents changed in the
// last 30 days count. Business acronyms and product names are what
// recognition gets wrong; the workspaces are where the user's own ones are written down.
func Glossary(root string, workspaces []ClueWorkspace, max int, now time.Time) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		t = strings.TrimSpace(t)
		k := strings.ToLower(t)
		if t == "" || seen[k] || len(out) >= max {
			return
		}
		seen[k] = true
		out = append(out, t)
	}
	if f, err := os.Open(filepath.Join(root, GlossaryFile)); err == nil {
		sc := bufio.NewScanner(io.LimitReader(f, maxDocBytes))
		for sc.Scan() {
			line, _, _ := strings.Cut(sc.Text(), "#")
			add(line)
		}
		f.Close()
	}
	// tf is each term's share of its workspace's terms, summed over workspaces: every workspace
	// weighs the same however long its documents are.
	tf := map[string]float64{}
	// df is how many workspaces a term occurs in: one every project uses (VPC, AWS) says little
	// about this user's meetings, one only a project of theirs uses (a product or team name) a lot.
	df := map[string]int{}
	var recent []ClueWorkspace
	for _, w := range workspaces {
		used := false
		in := map[string]int{}
		for _, doc := range glossaryDocs {
			p := filepath.Join(w.Path, doc)
			st, err := os.Stat(p)
			if err != nil || now.Sub(st.ModTime()) > recentDocs {
				continue
			}
			b, err := readCapped(p)
			if err != nil {
				continue
			}
			used = true
			text := codeRE.ReplaceAllString(string(b), " ")
			for _, t := range termRE.FindAllString(text, -1) {
				if nameLike(t) && !commonTerms[strings.ToUpper(t)] {
					in[t]++
				}
			}
		}
		if used {
			recent = append(recent, w)
		}
		total := 0
		for _, c := range in {
			total += c
		}
		for t, c := range in {
			df[t]++
			tf[t] += float64(c) / float64(total)
		}
	}
	score := map[string]float64{}
	for t, f := range tf {
		score[t] = f * math.Log(1+float64(len(recent))/float64(df[t]))
	}
	terms := make([]string, 0, len(score))
	for t := range score {
		terms = append(terms, t)
	}
	sort.Slice(terms, func(i, j int) bool {
		if score[terms[i]] != score[terms[j]] {
			return score[terms[i]] > score[terms[j]]
		}
		return terms[i] < terms[j]
	})
	// The workspace names keep a fifth of the room: a meeting is often about one of them.
	room := max - max/5
	for _, t := range terms {
		if len(out) >= room {
			break
		}
		add(t)
	}
	for _, w := range recent {
		add(w.Name)
	}
	for _, t := range terms {
		add(t)
	}
	return out
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxDocBytes))
}

// matchWorkspaces is the workspaces a meeting names in its title or attendees. Terms from every
// workspace on the disk drown the few that matter in generic ones (measured on the owner's
// 40-odd workspaces), so documents are read only for the workspaces the meeting points at.
func matchWorkspaces(all []ClueWorkspace, mt Meeting) []ClueWorkspace {
	text := strings.ToLower(mt.Title + " " + strings.Join(mt.Attendees, " "))
	var out []ClueWorkspace
	for _, w := range all {
		name := strings.ToLower(strings.TrimSpace(w.Name))
		if len(name) >= 2 && strings.Contains(text, name) {
			out = append(out, w)
		}
	}
	return out
}
