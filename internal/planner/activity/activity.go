// Package activity derives the Planner's Activity (state change) records from
// a session's tool events, deterministically and without asking the model
// (docs/rfc/planner.md §3.4, §10.2, §15.1 W1): pushes, tags, PRs opened and
// merged, releases, AWS writes, sent mail, Asana writes, and the output files
// a session wrote. Records go to the Planner journal as kind=activity; the
// outputs among them (files and PRs) also feed an in-memory index that is
// rebuilt from the journal on start (Recorder).
package activity

import (
	"cmp"
	"encoding/json"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/naozhi/naozhi/internal/textutil"
)

// Type classifies an Activity.
type Type string

const (
	TypeGitPush  Type = "git_push"
	TypeGitTag   Type = "git_tag"
	TypePRCreate Type = "pr_create"
	TypePRMerge  Type = "pr_merge"
	// TypePR is a pull request a tool result linked to without the call
	// itself being a create or merge (git push output, gh pr view).
	TypePR       Type = "pr"
	TypeRelease  Type = "release"
	TypeAWSWrite Type = "aws_write"
	TypeEmail    Type = "email"
	TypeAsana    Type = "asana"
	// TypeOutput is an output file (IsOutputFile) the session wrote.
	TypeOutput Type = "output"
)

// PR states, when the event says which.
const (
	StateOpened = "opened"
	StateMerged = "merged"
)

// Activity is one state change. It is the data of a kind=activity journal
// entry.
type Activity struct {
	Type    Type   `json:"type"`
	Summary string `json:"summary"`
	URL     string `json:"url,omitempty"`
	State   string `json:"state,omitempty"`
	// Path is an output file relative to the session's file root, the
	// path= of a root=session:<key> file-API request.
	Path string `json:"path,omitempty"`
}

// IsOutput reports whether a lands in the outputs index: a file or a PR.
func (a Activity) IsOutput() bool {
	switch a.Type {
	case TypeOutput, TypePRCreate, TypePRMerge, TypePR:
		return true
	}
	return false
}

const maxSummaryRunes = 160

// outputExts are the extensions of files a session hands back as a product
// rather than as source (§15.1 W1).
var outputExts = map[string]bool{
	".html": true, ".md": true, ".pdf": true, ".png": true, ".svg": true,
	".pptx": true, ".xlsx": true, ".docx": true, ".csv": true,
}

// IsOutputFile reports whether name has an output extension.
func IsOutputFile(name string) bool {
	return outputExts[strings.ToLower(filepath.Ext(name))]
}

// RelPath returns abs relative to root in slash form, false when abs is not
// absolute or lies outside root (or is root itself). When the spellings
// differ only by a symlink (a workspace stored resolved, /private/tmp/w, and
// a tool writing to /tmp/w/a.md) both sides are compared resolved; abs need
// not exist yet.
func RelPath(root, abs string) (string, bool) {
	if root == "" || !filepath.IsAbs(abs) {
		return "", false
	}
	if rel, ok := relLexical(root, abs); ok {
		return rel, true
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	return relLexical(resolvedRoot, resolveExisting(abs))
}

func relLexical(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, filepath.Clean(abs))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// resolveExisting resolves the symlinks of p's longest existing ancestor and
// appends the rest.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// toolInput is the part of a tool_use input Classify reads.
type toolInput struct {
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Subject      string `json:"subject"`
	Name         string `json:"name"`
	Title        string `json:"title"`
}

// Classify reads one tool_use. path is the file a Write / Edit class tool
// writes, whatever its extension ("" for other tools). ok reports a state
// change; an output's Path is left absolute for the caller to make relative
// (RelPath).
func Classify(tool string, input json.RawMessage) (a Activity, path string, ok bool) {
	var in toolInput
	switch {
	case tool == "Bash":
		if json.Unmarshal(input, &in) != nil {
			return Activity{}, "", false
		}
		a, ok = FromCommand(in.Command)
		return a, "", ok
	case tool == "Write" || tool == "Edit" || tool == "MultiEdit" || tool == "NotebookEdit":
		if json.Unmarshal(input, &in) != nil {
			return Activity{}, "", false
		}
		path = in.FilePath
		if path == "" {
			path = in.NotebookPath
		}
		if path == "" || !IsOutputFile(path) {
			return Activity{}, path, false
		}
		return Activity{Type: TypeOutput, Summary: filepath.Base(path), Path: path}, path, true
	case strings.HasPrefix(tool, "mcp__"):
		typ, leaf, ok := classifyMCP(tool)
		if !ok {
			return Activity{}, "", false
		}
		summary := leaf
		if json.Unmarshal(input, &in) == nil {
			if t := cmp.Or(in.Subject, in.Name, in.Title); t != "" {
				summary += ": " + t
			}
		}
		return Activity{Type: typ, Summary: oneLine(summary)}, "", true
	}
	return Activity{}, "", false
}

// mcpEmailVerbs and mcpAsanaVerbs are the words that make an MCP tool a
// write. Drafting a mail is not sending one.
var (
	mcpEmailVerbs = wordSet("send", "reply", "forward")
	mcpAsanaVerbs = wordSet("create", "update", "delete", "add", "remove", "set", "save", "instantiate", "duplicate", "move")
)

// classifyMCP classifies an mcp__<server>__<tool> name: mail tools that send
// and Asana tools that write. leaf is the tool part.
func classifyMCP(name string) (Type, string, bool) {
	rest := strings.TrimPrefix(name, "mcp__")
	server, leaf, found := strings.Cut(rest, "__")
	if !found {
		return "", "", false
	}
	leaf = strings.TrimLeft(leaf, "_")
	server = strings.ToLower(server)
	words := splitWords(leaf)
	switch {
	case strings.Contains(server, "asana") || hasWord(words, "asana"):
		if anyWordIn(words, mcpAsanaVerbs) {
			return TypeAsana, trimServicePrefix(leaf), true
		}
	case strings.Contains(server, "mail") || strings.Contains(server, "outlook") ||
		hasWord(words, "email") || hasWord(words, "mail"):
		if anyWordIn(words, mcpEmailVerbs) {
			return TypeEmail, trimServicePrefix(leaf), true
		}
	}
	return "", "", false
}

// trimServicePrefix drops a "<service>___" namespace some gateways put in
// front of the tool name (asana___CreateTask).
func trimServicePrefix(leaf string) string {
	if i := strings.LastIndex(leaf, "___"); i >= 0 {
		return leaf[i+3:]
	}
	return leaf
}

// splitWords lowercases name split at '_', '-' and camelCase humps.
func splitWords(name string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		switch {
		case r == '_' || r == '-' || r == '.':
			flush()
		case unicode.IsUpper(r) && len(cur) > 0 && (unicode.IsLower(cur[len(cur)-1]) || i+1 < len(rs) && unicode.IsLower(rs[i+1])):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// prLinkRE matches a GitHub pull request URL.
var prLinkRE = regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]+`)

// maxPRLinks caps the links taken from one text: a listing is not a series
// of state changes.
const maxPRLinks = 5

// PRLinks returns the distinct GitHub PR URLs in text, first seen first.
func PRLinks(text string) []string {
	if !strings.Contains(text, "/pull/") {
		return nil
	}
	var out []string
	for _, u := range prLinkRE.FindAllString(text, -1) {
		if len(out) == maxPRLinks {
			break
		}
		if !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// FromCommand classifies a Bash command line: the first simple command in it
// that changes state wins.
func FromCommand(cmd string) (Activity, bool) {
	for _, words := range segments(cmd) {
		words = stripWrappers(words)
		if len(words) == 0 {
			continue
		}
		if a, ok := classifyWords(words); ok {
			return a, true
		}
	}
	return Activity{}, false
}

func classifyWords(words []string) (Activity, bool) {
	if hasWord(words, "--dry-run") || hasWord(words, "--dryrun") {
		return Activity{}, false
	}
	prog := path.Base(filepath.ToSlash(words[0]))
	switch prog {
	case "git":
		return classifyGit(words)
	case "gh":
		return classifyGH(words)
	case "aws":
		return classifyAWS(words)
	case "make", "gmake":
		for _, t := range makeTargets(words[1:]) {
			if strings.HasPrefix(t, "release") || strings.HasPrefix(t, "publish") {
				return Activity{Type: TypeRelease, Summary: summarize(words)}, true
			}
		}
	case "goreleaser":
		pos := positionals(words[1:])
		if !hasWord(words, "--snapshot") && (len(pos) == 0 || pos[0] == "release") {
			return Activity{Type: TypeRelease, Summary: summarize(words)}, true
		}
	case "npm", "pnpm", "yarn", "cargo":
		if len(words) > 1 && words[1] == "publish" {
			return Activity{Type: TypeRelease, Summary: summarize(words)}, true
		}
	case "bash", "sh", "zsh":
		if len(words) > 1 && isReleaseScript(words[1]) {
			return Activity{Type: TypeRelease, Summary: summarize(words)}, true
		}
	default:
		if isReleaseScript(words[0]) {
			return Activity{Type: TypeRelease, Summary: summarize(words)}, true
		}
	}
	return Activity{}, false
}

// isReleaseScript reports a script (a path or a *.sh) named release* or
// publish*: scripts/release.sh, ./publish.
func isReleaseScript(word string) bool {
	if !strings.Contains(word, "/") && !strings.HasSuffix(word, ".sh") {
		return false
	}
	base := strings.ToLower(path.Base(filepath.ToSlash(word)))
	return strings.HasPrefix(base, "release") || strings.HasPrefix(base, "publish")
}

// gitValueOpts are git's global options that take the next word.
var gitValueOpts = wordSet("-C", "-c", "--git-dir", "--work-tree", "--namespace", "--exec-path")

// gitTagReadOpts make git tag list or verify rather than create or delete.
var gitTagReadOpts = wordSet("-l", "--list", "-v", "--verify", "--contains", "--no-contains", "--points-at", "--merged", "--no-merged")

func classifyGit(words []string) (Activity, bool) {
	i := 1
	for i < len(words) && strings.HasPrefix(words[i], "-") {
		if gitValueOpts[words[i]] {
			i++
		}
		i++
	}
	if i >= len(words) {
		return Activity{}, false
	}
	sub, args := words[i], words[i+1:]
	summary := summarize(append([]string{"git"}, words[i:]...))
	switch sub {
	case "push":
		if hasWord(args, "-n") {
			return Activity{}, false
		}
		return Activity{Type: TypeGitPush, Summary: summary}, true
	case "tag":
		for _, a := range args {
			if gitTagReadOpts[a] || strings.HasPrefix(a, "-n") || strings.HasPrefix(a, "--sort") || strings.HasPrefix(a, "--format") {
				return Activity{}, false
			}
		}
		if len(positionals(args)) == 0 {
			return Activity{}, false
		}
		return Activity{Type: TypeGitTag, Summary: summary}, true
	}
	return Activity{}, false
}

// ghReleaseWrites are the gh release subcommands that change a release.
var ghReleaseWrites = wordSet("create", "edit", "delete", "upload", "delete-asset")

func classifyGH(words []string) (Activity, bool) {
	if len(words) < 3 {
		return Activity{}, false
	}
	switch words[1] + " " + words[2] {
	case "pr create":
		return Activity{Type: TypePRCreate, Summary: summarize(words[:3]), State: StateOpened}, true
	case "pr merge":
		a := Activity{Type: TypePRMerge, Summary: summarize(words), State: StateMerged}
		for _, w := range words[3:] {
			if prLinkRE.MatchString(w) {
				a.URL = prLinkRE.FindString(w)
				break
			}
		}
		return a, true
	}
	if words[1] == "release" && ghReleaseWrites[words[2]] {
		return Activity{Type: TypeRelease, Summary: summarize(words)}, true
	}
	return Activity{}, false
}

// awsValueOpts are the aws CLI global options that take the next word.
var awsValueOpts = wordSet("--region", "--profile", "--output", "--endpoint-url", "--query",
	"--color", "--ca-bundle", "--cli-read-timeout", "--cli-connect-timeout", "--cli-binary-format")

// awsWriteVerbs are the leading words of AWS API operations that change
// state. Anything else (describe, get, list, scan, query, wait, assume, ...)
// reads; an unknown verb counts as a read.
var awsWriteVerbs = wordSet("create", "put", "delete", "update", "run", "start", "stop", "terminate",
	"modify", "attach", "detach", "associate", "disassociate", "register", "deregister", "invoke",
	"publish", "send", "tag", "untag", "reboot", "restore", "copy", "import", "enable", "disable",
	"set", "add", "remove", "cancel", "deploy", "execute", "apply", "reset", "revoke", "authorize",
	"upload", "replace", "rotate", "change", "reject", "accept", "allocate", "release", "purchase",
	"request", "complete", "abort", "write", "increase", "decrease", "merge", "promote", "restart",
	"resume", "suspend", "rollback", "transfer", "unassign", "assign", "subscribe", "unsubscribe", "admin")

func classifyAWS(words []string) (Activity, bool) {
	var pos []string
	for i := 1; i < len(words); i++ {
		w := words[i]
		if strings.HasPrefix(w, "--") {
			if len(pos) == 0 && awsValueOpts[w] {
				i++
			}
			continue
		}
		pos = append(pos, w)
	}
	if len(pos) < 2 {
		return Activity{}, false
	}
	service, op := pos[0], pos[1]
	if service == "configure" || service == "help" || op == "help" {
		return Activity{}, false
	}
	a := Activity{Type: TypeAWSWrite, Summary: "aws " + service + " " + op}
	if service == "s3" {
		switch op {
		case "rm", "mb", "rb":
			return a, true
		case "cp", "mv", "sync":
			// Writes when the destination (the last path) is a bucket.
			if len(pos) >= 4 && strings.HasPrefix(pos[len(pos)-1], "s3://") {
				return a, true
			}
		}
		return Activity{}, false
	}
	verb, rest, _ := strings.Cut(op, "-")
	if verb == "batch" || verb == "admin" {
		// batch-get-item reads, batch-write-item writes.
		verb, _, _ = strings.Cut(rest, "-")
	}
	if awsWriteVerbs[verb] {
		return a, true
	}
	return Activity{}, false
}

// makeValueOpts are make options that take the next word.
var makeValueOpts = wordSet("-C", "-f", "-I", "-o", "-W", "--directory", "--file", "--makefile")

// makeTargets returns the goals of a make command line, skipping options
// and VAR=value overrides.
func makeTargets(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case makeValueOpts[a]:
			i++
		case strings.HasPrefix(a, "-") || strings.Contains(a, "="):
		default:
			out = append(out, a)
		}
	}
	return out
}

// positionals returns the words that are not options.
func positionals(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// wrapperProgs run the command that follows them.
var wrapperProgs = wordSet("sudo", "env", "command", "exec", "nohup", "time", "nice", "caffeinate")

// stripWrappers drops leading VAR=value assignments and wrapper programs
// (sudo, env, timeout 90, ...) with their options, leaving the command they
// run.
func stripWrappers(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		switch {
		case isAssignment(w):
			words = words[1:]
		case wrapperProgs[path.Base(w)]:
			words = words[1:]
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || isAssignment(words[0])) {
				words = words[1:]
			}
		case path.Base(w) == "timeout":
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				words = words[1:]
			}
			if len(words) > 0 {
				words = words[1:] // the duration
			}
		default:
			return words
		}
	}
	return words
}

func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if r != '_' && !unicode.IsLetter(r) && (i == 0 || !unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

// segments splits a shell command line into simple commands, each a list of
// words: quotes and backslashes are honoured, and ; & | newlines and
// parentheses end a command. It finds a command's program and subcommand; it
// is not a shell parser (no expansion, no here-docs).
func segments(cmd string) [][]string {
	var (
		out     [][]string
		words   []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
		comment bool
	)
	flushWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	flushCmd := func() {
		flushWord()
		if len(words) > 0 {
			out = append(out, words)
			words = nil
		}
	}
	for _, r := range cmd {
		switch {
		case comment:
			if r == '\n' {
				comment = false
				flushCmd()
			}
		case escaped:
			escaped = false
			if r != '\n' {
				cur.WriteRune(r)
				inWord = true
			}
		case quote != 0:
			switch {
			case r == quote:
				quote = 0
			case r == '\\' && quote == '"':
				escaped = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped = true
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == '#' && !inWord:
			comment = true
		case r == ';' || r == '&' || r == '|' || r == '\n' || r == '(' || r == ')':
			flushCmd()
		case unicode.IsSpace(r):
			flushWord()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	flushCmd()
	return out
}

func summarize(words []string) string {
	return oneLine(strings.Join(words, " "))
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return textutil.TruncateRunes(s, maxSummaryRunes)
}

func wordSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

func hasWord(words []string, w string) bool { return slices.Contains(words, w) }

func anyWordIn(words []string, set map[string]bool) bool {
	for _, w := range words {
		if set[w] {
			return true
		}
	}
	return false
}
