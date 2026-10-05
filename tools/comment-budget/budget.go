package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// Counts is the comment budget of a tree: how many comments break each of
// CLAUDE.md's Code Comments rules. Each may only go down.
type Counts struct {
	// InFuncBlocksOver5 counts comment blocks inside a function body longer
	// than 5 lines.
	InFuncBlocksOver5 int
	// DocOver10 counts godoc on one identifier longer than 10 lines.
	DocOver10 int
	// PackageDocOver60 counts package docs longer than 60 lines.
	PackageDocOver60 int
	// ReviewAnchors counts cron-cr finding IDs (R<nnn>-<AREA>-<n>).
	ReviewAnchors int
	// HistoryPhrases counts phrases that narrate what the code used to do.
	HistoryPhrases int
	// DuplicateComments counts adjacent comment blocks with identical text,
	// separated by exactly one blank line (a stray copy left by a split).
	DuplicateComments int
	// MisplacedDocs counts godocs off the declaration they describe: one
	// opening with another top-level name of its package or with an
	// identifier-shaped name nothing declares, or a top-level block after a
	// file's last declaration, which documents nothing.
	MisplacedDocs int
	// IssueRefs counts #NNNN references.
	IssueRefs int
	// Offenders maps each counter to its file:line hits, for the report.
	Offenders map[string][]string
}

var (
	reviewAnchorRe  = regexp.MustCompile(`\bR\d{3,}[a-z]?(?:-[A-Z0-9]+)+-\d+\b`)
	historyPhraseRe = regexp.MustCompile(`(?i)\b(used to (?:be|live|have|return|run|take|keep|hold)|was removed|were removed|historically|historical note|formerly)\b|以前|原先|旧逻辑|旧实现`)
	issueRefRe      = regexp.MustCompile(`#\d{3,5}\b`)
)

// count walks root's non-test, non-generated Go files.
func count(root string) (Counts, error) {
	c := Counts{Offenders: map[string][]string{}}
	fset := token.NewFileSet()
	pkgs := map[string][]*ast.File{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if testhelper.SkipRepoDir(root, path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		f, err := countFile(&c, fset, rel, src)
		if f != nil {
			pkgs[filepath.Dir(rel)] = append(pkgs[filepath.Dir(rel)], f)
		}
		return err
	})
	for _, files := range pkgs {
		countMisplacedDocs(&c, fset, files)
	}
	for k := range c.Offenders {
		sort.Strings(c.Offenders[k])
	}
	return c, err
}

// countFile counts src's per-file counters and returns its parse, nil for a
// generated file; countMisplacedDocs needs the whole package.
func countFile(c *Counts, fset *token.FileSet, rel string, src []byte) (*ast.File, error) {
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	if ast.IsGenerated(f) {
		return nil, nil
	}
	hit := func(key string, pos token.Pos) { c.hit(fset, key, pos) }
	lines := func(g *ast.CommentGroup) int {
		return fset.Position(g.End()).Line - fset.Position(g.Pos()).Line + 1
	}
	srcLines := strings.Split(string(src), "\n")
	blankLine := func(n int) bool { return n >= 1 && n <= len(srcLines) && strings.TrimSpace(srcLines[n-1]) == "" }

	docs := map[*ast.CommentGroup]bool{}
	if f.Doc != nil {
		docs[f.Doc] = true
		if lines(f.Doc) > 60 {
			c.PackageDocOver60++
			hit("PackageDocOver60", f.Doc.Pos())
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		var doc *ast.CommentGroup
		switch n := n.(type) {
		case *ast.FuncDecl:
			doc = n.Doc
		case *ast.GenDecl:
			doc = n.Doc
		case *ast.TypeSpec:
			doc = n.Doc
		case *ast.ValueSpec:
			doc = n.Doc
		case *ast.Field:
			doc = n.Doc
		}
		if doc != nil && !docs[doc] {
			docs[doc] = true
			if lines(doc) > 10 {
				c.DocOver10++
				hit("DocOver10", doc.Pos())
			}
		}
		return true
	})
	var bodies [][2]token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if n.Body != nil {
				bodies = append(bodies, [2]token.Pos{n.Body.Lbrace, n.Body.Rbrace})
			}
		case *ast.FuncLit:
			bodies = append(bodies, [2]token.Pos{n.Body.Lbrace, n.Body.Rbrace})
		}
		return true
	})
	inBody := func(p token.Pos) bool {
		for _, b := range bodies {
			if p > b[0] && p < b[1] {
				return true
			}
		}
		return false
	}
	for _, g := range f.Comments {
		if !docs[g] && inBody(g.Pos()) && lines(g) > 5 {
			c.InFuncBlocksOver5++
			hit("InFuncBlocksOver5", g.Pos())
		}
		for _, cm := range g.List {
			text := cm.Text
			if strings.HasPrefix(text, "//go:") || strings.HasPrefix(text, "//nolint") {
				continue
			}
			if n := len(reviewAnchorRe.FindAllString(text, -1)); n > 0 {
				c.ReviewAnchors += n
				hit("ReviewAnchors", cm.Pos())
			}
			if n := len(historyPhraseRe.FindAllString(text, -1)); n > 0 {
				c.HistoryPhrases += n
				hit("HistoryPhrases", cm.Pos())
			}
			c.IssueRefs += len(issueRefRe.FindAllString(text, -1))
		}
	}
	for i := 0; i+1 < len(f.Comments); i++ {
		prev, next := f.Comments[i], f.Comments[i+1]
		gapLine := fset.Position(prev.End()).Line + 1
		if fset.Position(next.Pos()).Line != gapLine+1 || !blankLine(gapLine) {
			continue
		}
		if isGoDirective(prev) || isGoDirective(next) || !sameCommentText(prev, next) {
			continue
		}
		c.DuplicateComments++
		hit("DuplicateComments", prev.Pos())
	}
	return f, nil
}

func (c *Counts) hit(fset *token.FileSet, key string, pos token.Pos) {
	p := fset.Position(pos)
	c.Offenders[key] = append(c.Offenders[key], p.Filename+":"+itoa(p.Line))
}

// countMisplacedDocs counts MisplacedDocs over the files of one package. A
// doc may open with its own name or a prefix of it (a family doc); prose
// openers are legal because only declared or identifier-shaped words count.
// A file with no declarations is a design note and is skipped.
func countMisplacedDocs(c *Counts, fset *token.FileSet, files []*ast.File) {
	names := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			for _, n := range declNames(d) {
				names[n] = true
			}
		}
	}
	misplaced := func(doc *ast.CommentGroup, own []string) {
		w := docSubject(doc)
		if w == "" || slices.Contains(own, w) || !slices.ContainsFunc(own, func(n string) bool { return n != "_" }) {
			return
		}
		if !names[w] && (!identShaped(w) || slices.ContainsFunc(own, func(n string) bool { return strings.HasPrefix(n, w) })) {
			return
		}
		c.MisplacedDocs++
		c.hit(fset, "MisplacedDocs", doc.Pos())
	}
	for _, f := range files {
		if len(f.Decls) == 0 {
			continue
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Doc != nil {
					misplaced(d.Doc, declNames(d))
				}
			case *ast.GenDecl:
				if d.Doc != nil && len(d.Specs) == 1 {
					misplaced(d.Doc, declNames(d))
				}
				for _, s := range d.Specs {
					if doc := specDoc(s); doc != nil {
						misplaced(doc, declNames(d))
					}
				}
			}
		}
		last := fset.Position(f.Decls[len(f.Decls)-1].End()).Line
		for _, g := range f.Comments {
			if fset.Position(g.Pos()).Line > last && !isGoDirective(g) {
				c.MisplacedDocs++
				c.hit(fset, "MisplacedDocs", g.Pos())
			}
		}
	}
}

// identShaped reports whether w reads as a Go identifier rather than a word:
// a lower-to-upper case step (fooBar, GetFoo) or a lower-case start with an
// underscore.
func identShaped(w string) bool {
	r := []rune(w)
	for i := 1; i < len(r); i++ {
		if unicode.IsLower(r[i-1]) && unicode.IsUpper(r[i]) {
			return true
		}
	}
	return len(r) > 0 && unicode.IsLower(r[0]) && strings.Contains(w, "_")
}

// declNames lists the names d declares; a method declares its own name.
func declNames(d ast.Decl) []string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		return []string{d.Name.Name}
	case *ast.GenDecl:
		var out []string
		for _, s := range d.Specs {
			out = append(out, specNames(s)...)
		}
		return out
	}
	return nil
}

func specNames(s ast.Spec) []string {
	switch s := s.(type) {
	case *ast.TypeSpec:
		return []string{s.Name.Name}
	case *ast.ValueSpec:
		var out []string
		for _, n := range s.Names {
			out = append(out, n.Name)
		}
		return out
	}
	return nil
}

func specDoc(s ast.Spec) *ast.CommentGroup {
	switch s := s.(type) {
	case *ast.TypeSpec:
		return s.Doc
	case *ast.ValueSpec:
		return s.Doc
	}
	return nil
}

// docSubject returns the identifier doc opens with, the last part of a
// dotted Type.Method, or "" when it opens with something else.
func docSubject(doc *ast.CommentGroup) string {
	text := strings.TrimLeft(doc.Text(), " \t\n")
	i := strings.IndexFunc(text, func(r rune) bool {
		return r != '.' && r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if i >= 0 {
		text = text[:i]
	}
	text = strings.TrimRight(text, ".")
	return text[strings.LastIndexByte(text, '.')+1:]
}

// isGoDirective reports whether every line of g is a //go: directive.
func isGoDirective(g *ast.CommentGroup) bool {
	for _, cm := range g.List {
		if !strings.HasPrefix(cm.Text, "//go:") {
			return false
		}
	}
	return true
}

// sameCommentText reports whether a and b hold the same lines, in order.
func sameCommentText(a, b *ast.CommentGroup) bool {
	if len(a.List) != len(b.List) {
		return false
	}
	for i, cm := range a.List {
		if cm.Text != b.List[i].Text {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
