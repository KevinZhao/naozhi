package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

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
		return countFile(&c, rel, src)
	})
	for k := range c.Offenders {
		sort.Strings(c.Offenders[k])
	}
	return c, err
}

func countFile(c *Counts, rel string, src []byte) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	if ast.IsGenerated(f) {
		return nil
	}
	at := func(pos token.Pos) string { return rel + ":" + itoa(fset.Position(pos).Line) }
	hit := func(key string, pos token.Pos) { c.Offenders[key] = append(c.Offenders[key], at(pos)) }
	lines := func(g *ast.CommentGroup) int {
		return fset.Position(g.End()).Line - fset.Position(g.Pos()).Line + 1
	}

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
	return nil
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
