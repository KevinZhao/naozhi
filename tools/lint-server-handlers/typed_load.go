package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// typedPkg is one type-checked package of the main module (non-test files).
type typedPkg struct {
	Path  string // import path
	Rel   string // import path relative to the module ("internal/server")
	Files []*ast.File
	Info  *types.Info
}

// typedProgram is every package of the module rooted at Root, type-checked
// against the export data `go list -export` produced for its dependencies.
type typedProgram struct {
	Root   string
	Module string
	Fset   *token.FileSet
	Pkgs   []*typedPkg
}

type goListPkg struct {
	ImportPath string
	Dir        string
	Export     string
	GoFiles    []string
	CgoFiles   []string
	DepOnly    bool
	Module     *struct {
		Path string
		Main bool
	}
	Error *struct{ Err string }
}

// loadTyped type-checks the module at root. It never skips quietly: a go list
// failure, a type error, a cgo file it cannot check, or a checked set that
// differs from `go list ./...` is an error, because a rule that loses a package
// passes everything in it.
func loadTyped(root string) (*typedProgram, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	out, err := runGo(root, "list", "-export", "-deps", "-json=ImportPath,Dir,Export,GoFiles,CgoFiles,DepOnly,Module,Error", "./...")
	if err != nil {
		return nil, err
	}
	exports := map[string]string{}
	var mods []goListPkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p goListPkg
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		if p.Error != nil {
			return nil, fmt.Errorf("go list %s: %s", p.ImportPath, p.Error.Err)
		}
		exports[p.ImportPath] = p.Export
		if p.Module != nil && p.Module.Main && !p.DepOnly {
			mods = append(mods, p)
		}
	}
	if len(mods) == 0 {
		return nil, fmt.Errorf("go list ./... in %s found no packages of the main module", root)
	}
	if err := sameAsGoList(root, mods); err != nil {
		return nil, err
	}

	prog := &typedProgram{Root: root, Module: mods[0].Module.Path, Fset: token.NewFileSet()}
	imp := importer.ForCompiler(prog.Fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	})
	for _, p := range mods {
		if len(p.CgoFiles) > 0 {
			return nil, fmt.Errorf("%s has cgo files %v: the typed loader does not check them", p.ImportPath, p.CgoFiles)
		}
		tp, err := checkPackage(prog, imp, p)
		if err != nil {
			return nil, err
		}
		prog.Pkgs = append(prog.Pkgs, tp)
	}
	return prog, nil
}

func checkPackage(prog *typedProgram, imp types.Importer, p goListPkg) (*typedPkg, error) {
	tp := &typedPkg{Path: p.ImportPath, Rel: relToModule(prog.Module, p.ImportPath)}
	for _, name := range p.GoFiles {
		f, err := parser.ParseFile(prog.Fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		tp.Files = append(tp.Files, f)
	}
	tp.Info = &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Instances:  map[*ast.Ident]types.Instance{},
	}
	var errs []string
	conf := types.Config{Importer: imp, Error: func(err error) { errs = append(errs, err.Error()) }}
	_, _ = conf.Check(p.ImportPath, prog.Fset, tp.Files, tp.Info)
	if len(errs) > 0 {
		return nil, fmt.Errorf("type-check %s: %s", p.ImportPath, strings.Join(errs, "; "))
	}
	return tp, nil
}

// sameAsGoList compares the packages about to be checked with a plain
// `go list ./...`, so a filter bug in the -deps pass cannot drop a package.
func sameAsGoList(root string, mods []goListPkg) error {
	out, err := runGo(root, "list", "./...")
	if err != nil {
		return err
	}
	want := strings.Fields(string(out))
	var got []string
	for _, p := range mods {
		got = append(got, p.ImportPath)
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(want, "\n") != strings.Join(got, "\n") {
		return fmt.Errorf("typed loader would check %d packages but go list ./... names %d", len(got), len(want))
	}
	return nil
}

func runGo(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return out, nil
}

// relToModule strips the module path from an import path or a
// types.Func.FullName, so rule tables read "internal/server" and fixture
// modules can reuse them.
func relToModule(module, s string) string {
	s = strings.ReplaceAll(s, module+"/", "")
	if s == module {
		return "."
	}
	return s
}
