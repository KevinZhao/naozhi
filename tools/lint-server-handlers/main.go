// Command lint-server-handlers enforces naozhi's server-package contracts
// defined in docs/design/server-split-phase4-design.md §六.2 / §九.2:
//
//   - Rule 1 (handle_decl): no new `func (s *Server) handle*` methods
//     after Phase 0. The full Phase 0 baseline of existing handle*
//     methods is auto-baselined at first run; future PRs that add a
//     new handle* method to *Server fail CI.
//   - Rule 2 (file_size): files in internal/server/ ≤ 500 lines
//     (non-test); internal/dashboard/*/  ≤ 800 lines (non-test).
//     Existing over-limit files are listed in exemptions.yaml with an
//     `until_phase` field; the linter ignores them until the listed
//     phase has merged. New files are always checked.
//   - Rule 3a (field_block markers, Phase 0 必交付): wshub_*.go godoc
//     头必须含 Field-block contract / WRITES: / READS-ALSO: /
//     LIFECYCLE-METHOD 标注。文本扫描，不做语义分析。
//   - Rule 3b (field_block AST, Phase 4b 前) — TODO: 字段访问对账
//     与 §五 7 块归属表 + 跨方法追踪。
//   - Rule 4 (iface_match, Phase 0 框架 / Phase 1 前完整): 实现侧
//     godoc 中的 satisfies: 注释必须在 consumer-contracts.md 有对应
//     条目。Phase 0b 骨架仅扫接口名出现性；Phase 1 前升级 method-set 对账。
//   - Rule 5 (stale_exemption, Phase 0 框架 / Phase 1 前完整): exemptions
//     条目反向依赖检查。Phase 0b 骨架仅检查文件存在；Phase 1 前补 git
//     tag 比对 + 行数比对 + Closes-exemption commit trailer 校验。
//
// Modes:
//
//   - mode=warn (default): print violations to stderr, exit 0. CI uses
//     this during Phase 0-4 so existing exemptions don't block PRs.
//   - mode=fail: exit 1 on any violation. Phase 5 verification gate.
//
// Output format: human-readable lines on stderr; SARIF on stdout when
// -sarif is given. SARIF is the format GitHub PR Annotations expect.
//
// Usage:
//
//	lint-server-handlers [-mode warn|fail] [-sarif] [-exemptions PATH]
//
// Run from the repo root.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type mode int

const (
	modeWarn mode = iota
	modeFail
)

type Violation struct {
	Rule    string // handle_decl / file_size / field_block / iface_match
	File    string
	Line    int
	Message string
}

type exemption struct {
	Path       string `yaml:"path"`
	Current    int    `yaml:"current"`
	Limit      int    `yaml:"limit"`
	UntilPhase string `yaml:"until_phase"`
}

type exemptions struct {
	FileSize       []exemption `yaml:"file_size"`
	HandleBaseline []string    `yaml:"handle_baseline"` // pkg-qualified handler names exempted from rule 1
}

func main() {
	var (
		runMode      = flag.String("mode", "warn", "warn | fail")
		sarif        = flag.Bool("sarif", false, "emit SARIF on stdout")
		exemptPath   = flag.String("exemptions", "tools/lint-server-handlers/exemptions.yaml", "path to exemptions.yaml")
		genBaseline  = flag.Bool("gen-baseline", false, "(re)generate handle_baseline section of exemptions.yaml from current source and exit")
		serverPkg    = flag.String("server-pkg", "internal/server", "server package directory")
		dashboardPkg = flag.String("dashboard-pkg", "internal/dashboard", "dashboard package directory (may not exist yet)")
	)
	flag.Parse()

	m := modeWarn
	if *runMode == "fail" {
		m = modeFail
	}

	exempts, err := loadExemptions(*exemptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load exemptions %s: %v\n", *exemptPath, err)
		os.Exit(2)
	}

	if *genBaseline {
		names, err := scanHandleHandlers(*serverPkg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gen-baseline: %v\n", err)
			os.Exit(2)
		}
		exempts.HandleBaseline = names
		if err := saveExemptions(*exemptPath, exempts); err != nil {
			fmt.Fprintf(os.Stderr, "save: %v\n", err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "baseline: %d Server.handle* methods recorded\n", len(names))
		return
	}

	var vs []Violation

	// Rule 1: handle_decl
	currentHandlers, err := scanHandleHandlers(*serverPkg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan %s: %v\n", *serverPkg, err)
		os.Exit(2)
	}
	baseline := make(map[string]struct{}, len(exempts.HandleBaseline))
	for _, n := range exempts.HandleBaseline {
		baseline[n] = struct{}{}
	}
	for _, h := range currentHandlers {
		if _, ok := baseline[h]; ok {
			continue
		}
		vs = append(vs, Violation{
			Rule:    "handle_decl",
			File:    *serverPkg + "/server.go",
			Message: fmt.Sprintf("new Server.handle* method %q is forbidden after Phase 0; move it to a dashboard sub-package or add to exemptions.yaml handle_baseline (with justification)", h),
		})
	}

	// Rule 2: file_size
	exemptFiles := make(map[string]exemption, len(exempts.FileSize))
	for _, e := range exempts.FileSize {
		exemptFiles[e.Path] = e
	}
	vs = append(vs, scanFileSize(*serverPkg, 500, exemptFiles)...)
	if _, err := os.Stat(*dashboardPkg); err == nil {
		vs = append(vs, scanFileSize(*dashboardPkg, 800, exemptFiles)...)
	}

	// Rule 3a (Phase 0b 必交付): field_block godoc 标注扫描
	vs = append(vs, scanFieldBlockMarkers(*serverPkg)...)

	// Rule 4 (Phase 0b 框架, Phase 1 前完整): iface_match
	// 扫整仓 internal/ + cmd/ — 任何 godoc 含 satisfies: 注释的文件
	vs = append(vs, scanIfaceMatch([]string{"internal", "cmd"})...)

	// Rule 5 (Phase 0b 框架, Phase 1 前完整): stale_exemption
	vs = append(vs, scanStaleExemption(exempts)...)

	// Verbose mode: announce remaining skeleton work
	if os.Getenv("LINT_VERBOSE") == "1" {
		fmt.Fprintln(os.Stderr, "lint-server-handlers: rule 3b (AST field_block) due Phase 4b; rule 4 method-set 对账 + rule 5 git tag 对账 due Phase 1 (server-split-phase4-design.md v0.6.1 §六.2.0.4)")
	}

	if *sarif {
		emitSARIF(vs)
	} else {
		emitText(vs)
	}

	if len(vs) > 0 && m == modeFail {
		os.Exit(1)
	}
}

// scanHandleHandlers returns "Server.handleX" names for every method in
// the given server package directory whose receiver type is exactly
// *Server (or Server) AND whose name starts with "handle" or "Handle".
func scanHandleHandlers(pkgDir string) ([]string, error) {
	var out []string
	fset := token.NewFileSet()
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(pkgDir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			recv := fd.Recv.List[0]
			recvName := recvTypeName(recv.Type)
			if recvName != "Server" {
				continue
			}
			name := fd.Name.Name
			if !strings.HasPrefix(name, "handle") && !strings.HasPrefix(name, "Handle") {
				continue
			}
			out = append(out, "Server."+name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func scanFileSize(dir string, limit int, exempt map[string]exemption) []Violation {
	var out []Violation
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Normalise path separator for exemption-table match.
		rel := filepath.ToSlash(path)
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		buf := make([]byte, 64*1024)
		lines := 0
		var partial bool
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				for i := 0; i < n; i++ {
					if buf[i] == '\n' {
						lines++
						partial = false
					} else {
						partial = true
					}
				}
			}
			if rerr != nil {
				break
			}
		}
		if partial {
			lines++
		}
		if lines <= limit {
			return nil
		}
		if e, ok := exempt[rel]; ok {
			// Within budget: lines must not GROW beyond the recorded
			// baseline. New code in an exempted file is allowed to
			// stay or shrink; growing is a regression.
			if lines > e.Current {
				out = append(out, Violation{
					Rule:    "file_size",
					File:    rel,
					Message: fmt.Sprintf("%d lines (exemption baseline %d, limit %d, until_phase %s) — file grew, fix or update baseline", lines, e.Current, limit, e.UntilPhase),
				})
			}
			return nil
		}
		out = append(out, Violation{
			Rule:    "file_size",
			File:    rel,
			Message: fmt.Sprintf("%d lines exceeds %d (no exemption); split per server-split-phase4-design.md §四", lines, limit),
		})
		return nil
	})
	return out
}

func loadExemptions(path string) (*exemptions, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &exemptions{}, nil
		}
		return nil, err
	}
	var e exemptions
	if err := yaml.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func saveExemptions(path string, e *exemptions) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(e)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func emitText(vs []Violation) {
	if len(vs) == 0 {
		fmt.Fprintln(os.Stderr, "lint-server-handlers: no violations")
		return
	}
	for _, v := range vs {
		if v.Line > 0 {
			fmt.Fprintf(os.Stderr, "%s:%d: [%s] %s\n", v.File, v.Line, v.Rule, v.Message)
		} else {
			fmt.Fprintf(os.Stderr, "%s: [%s] %s\n", v.File, v.Rule, v.Message)
		}
	}
	fmt.Fprintf(os.Stderr, "lint-server-handlers: %d violation(s)\n", len(vs))
}

// emitSARIF prints a minimal SARIF 2.1.0 report on stdout. GitHub
// Actions consume this with codeql/upload-sarif. Keeping the producer
// inline avoids pulling sarif-go (1k-line dep) for one report shape.
func emitSARIF(vs []Violation) {
	const head = `{"$schema":"https://docs.oasis-open.org/sarif/sarif/v2.1.0/cos02/schemas/sarif-schema-2.1.0.json","version":"2.1.0","runs":[{"tool":{"driver":{"name":"lint-server-handlers","informationUri":"https://github.com/naozhi/naozhi/blob/master/docs/design/server-split-phase4-design.md","rules":[{"id":"handle_decl"},{"id":"file_size"},{"id":"field_block"},{"id":"iface_match"},{"id":"stale_exemption"}]}},"results":[`
	const tail = `]}]}`
	var sb strings.Builder
	sb.WriteString(head)
	for i, v := range vs {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb,
			`{"ruleId":%q,"level":"warning","message":{"text":%q},"locations":[{"physicalLocation":{"artifactLocation":{"uri":%q},"region":{"startLine":%d}}}]}`,
			v.Rule, v.Message, v.File, max1(v.Line))
	}
	sb.WriteString(tail)
	fmt.Println(sb.String())
}

func max1(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}
