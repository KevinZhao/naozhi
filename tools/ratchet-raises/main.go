// Command ratchet-raises fails a change that loosens a ratchet without an
// approved ledger entry (#2900). It reads every baseline at -base and in the
// working tree, lists the values that went up, and requires each to have a
// line appended to scripts/ratchet-raises.jsonl citing an issue that carries
// the ratchet-raise-approved label. Run from the repo root:
//
//	go run ./tools/ratchet-raises -base origin/master
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	jsRatchetPath  = "scripts/js-ratchet.baseline.json"
	jsCapsPath     = "scripts/js-ratchet.caps.json"
	jsDepsPath     = "scripts/js-deps-baseline.json"
	goldenPinsPath = "test/e2e/golden/pins.json"
	exemptionsPath = "tools/lint-server-handlers/exemptions.yaml"
	ledgerPath     = "scripts/ratchet-raises.jsonl"
)

func main() {
	base := flag.String("base", "", "git revision to compare against (required)")
	repo := flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name for the label check; empty skips it")
	flag.Parse()
	if *base == "" {
		fmt.Fprintln(os.Stderr, "ratchet-raises: -base is required")
		os.Exit(2)
	}
	problems, rs, err := run(gitTree{rev: *base}, workTree{}, labelsFromGH(*repo))
	if err != nil {
		fmt.Fprintln(os.Stderr, "ratchet-raises:", err)
		os.Exit(2)
	}
	for _, r := range rs {
		fmt.Printf("raise: %s %d -> %d\n", r.Gate, r.From, r.To)
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "ratchet-raises:", p)
		}
		os.Exit(1)
	}
	fmt.Printf("ratchet-raises: OK (%d approved raise(s))\n", len(rs))
}

// tree reads files at one revision.
type tree interface {
	read(path string) (string, error)
	// baselineGoFiles lists the .go files that declare a baseline constant.
	baselineGoFiles() ([]string, error)
}

func run(base, head tree, labels labelSource) ([]string, []raise, error) {
	bm, err := collect(base)
	if err != nil {
		return nil, nil, fmt.Errorf("base: %w", err)
	}
	hm, err := collect(head)
	if err != nil {
		return nil, nil, fmt.Errorf("head: %w", err)
	}
	// Once base has a pins document, every change to it is a raise, a pin head
	// adds included; the first document is free because golden metrics are
	// not newIsRaise on their own.
	basePins, err := base.read(goldenPinsPath)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(basePins) != "" {
		markNewIsRaise(hm, "golden:")
	}
	rs := raises(bm, hm)
	// Creating scripts/js-ratchet.caps.json establishes a new ratchet, not a
	// raise: its first exempt/legacy/cycle entries are already-real
	// violations being recorded. jsCaps parses one side at a time, so only
	// run() can tell "first document" from "entry added to it".
	baseCaps, err := base.read(jsCapsPath)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(baseCaps) == "" {
		rs = withoutPrefix(rs, "js-caps:")
	} else {
		var sections map[string]json.RawMessage
		if err := json.Unmarshal([]byte(baseCaps), &sections); err != nil {
			return nil, nil, fmt.Errorf("base: js-ratchet caps: %w", err)
		}
		for key, prefix := range capsSections {
			if _, ok := sections[key]; !ok {
				rs = withoutPrefix(rs, prefix)
			}
		}
	}
	bl, err := base.read(ledgerPath)
	if err != nil {
		return nil, nil, err
	}
	hl, err := head.read(ledgerPath)
	if err != nil {
		return nil, nil, err
	}
	return checkLedger(rs, bl, hl, labels), rs, nil
}

func collect(t tree) (metrics, error) {
	m := metrics{}
	paths, err := t.baselineGoFiles()
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, p := range paths {
		src, err := t.read(p)
		if err != nil {
			return nil, err
		}
		files[p] = src
	}
	goConsts(files, m)
	for path, parse := range map[string]func(string, metrics) error{
		jsRatchetPath: jsRatchet, jsCapsPath: jsCaps, jsDepsPath: jsDeps,
		goldenPinsPath: goldenPins, exemptionsPath: exemptions,
	} {
		raw, err := t.read(path)
		if err != nil {
			return nil, err
		}
		if err := parse(raw, m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// markNewIsRaise sets newIsRaise on every metric whose key starts with prefix.
func markNewIsRaise(m metrics, prefix string) {
	for k, v := range m {
		if strings.HasPrefix(k, prefix) {
			v.newIsRaise = true
			m[k] = v
		}
	}
}

// withoutPrefix drops every raise whose gate starts with prefix.
func withoutPrefix(rs []raise, prefix string) []raise {
	out := rs[:0:0]
	for _, r := range rs {
		if !strings.HasPrefix(r.Gate, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// gitTree reads a committed revision. A path absent there reads as "".
type gitTree struct{ rev string }

func (g gitTree) read(path string) (string, error) {
	out, err := exec.Command("git", "show", g.rev+":"+path).Output()
	if err != nil {
		if _, lsErr := exec.Command("git", "cat-file", "-e", g.rev+":"+path).Output(); lsErr != nil {
			return "", nil
		}
		return "", fmt.Errorf("git show %s:%s: %w", g.rev, path, err)
	}
	return string(out), nil
}

func (g gitTree) baselineGoFiles() ([]string, error) {
	return grepBaselineFiles("git", "grep", "-l", "-E", baselineGrep, g.rev, "--", "*.go")
}

// workTree reads the checkout.
type workTree struct{}

func (workTree) read(path string) (string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}

func (workTree) baselineGoFiles() ([]string, error) {
	return grepBaselineFiles("git", "grep", "-l", "-E", baselineGrep, "--", "*.go")
}

// baselineGrep preselects the files goBaselineConst then parses.
const baselineGrep = `aseline[A-Za-z0-9_]* *= *[0-9]`

func grepBaselineFiles(name string, args ...string) ([]string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil // no matches
		}
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return grepPaths(string(out)), nil
}

// grepPaths parses `git grep -l` output. With a revision git prints
// "<rev>:<path>"; in the working tree just "<path>".
func grepPaths(out string) []string {
	var paths []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		if i := strings.Index(l, ":"); i >= 0 {
			l = l[i+1:]
		}
		paths = append(paths, l)
	}
	return paths
}

// labelsFromGH asks the GitHub API through gh; nil (no check) without a repo.
func labelsFromGH(repo string) labelSource {
	if repo == "" {
		return nil
	}
	return func(issue int) ([]string, error) {
		out, err := exec.Command("gh", "api", "repos/"+repo+"/issues/"+strconv.Itoa(issue), "--jq", "[.labels[].name]").Output()
		if err != nil {
			return nil, fmt.Errorf("gh api: %w", err)
		}
		var names []string
		if err := json.Unmarshal(out, &names); err != nil {
			return nil, err
		}
		return names, nil
	}
}
