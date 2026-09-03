package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// nodeRegistryContractFile is excluded from the identifier scan below because
// it necessarily spells the banned names in its own ban list.
const nodeRegistryContractFile = "node_registry_contract_test.go"

// bannedNodeTableIdents are the pre-G2 spellings of the shared node table.
// Any of them reappearing as a Go identifier (field, var, option, selector)
// anywhere in internal/server means a caller has grown a second copy of the
// map/mutex outside *nodeRegistry — exactly the "three owners, lock by
// convention" shape #2192 removed. Comments are not scanned (they are not
// identifiers), so historical prose may still mention the old names.
var bannedNodeTableIdents = map[string]string{
	"nodesMu":         "raw node-table mutex; the registry owns the lock",
	"NodesMu":         "HubOptions mutex pointer; pass Nodes *nodeRegistry instead",
	"knownNodes":      "bare id→displayName map; use nodeRegistry.KnownNodes/SetKnown",
	"nodeAccessor":    "read-side wrapper folded into *nodeRegistry",
	"newNodeAccessor": "constructor of the folded wrapper",
}

// TestNodeRegistry_NoBareNodeTableIdentifiers is the source-level contract
// for G2 (RFC docs/rfc/godstruct-extraction.md §2.2 / #2192): every .go file
// in the package — tests included, since fixtures were the most frequent
// direct map pokers — must reach the node table exclusively through
// *nodeRegistry methods.
func TestNodeRegistry_NoBareNodeTableIdentifiers(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || name == nodeRegistryContractFile {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if why, banned := bannedNodeTableIdents[id.Name]; banned {
				violations = append(violations,
					fset.Position(id.Pos()).String()+": identifier "+id.Name+" ("+why+")")
			}
			return true
		})
	}
	if len(violations) > 0 {
		t.Errorf("bare node-table identifiers found outside *nodeRegistry (G2 / #2192):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestNodeRegistry_SingleOwnerFieldShape pins the type-level half of the
// contract with reflection: Server and Hub each hold exactly one
// *nodeRegistry and no bare `map[string]node.Conn` / id→name map, and
// HubOptions carries the registry rather than a map + mutex pair. The
// identifier scan above catches renamed-but-still-bare fields only if they
// reuse the old spelling; this catches the shape regardless of name.
func TestNodeRegistry_SingleOwnerFieldShape(t *testing.T) {
	t.Parallel()

	registryT := reflect.TypeOf((*nodeRegistry)(nil))
	connMapT := reflect.TypeOf(map[string]node.Conn(nil))
	mutexPtrT := reflect.TypeOf((*sync.RWMutex)(nil))

	check := func(name string, typ reflect.Type, wantRegistry int) {
		t.Helper()
		gotRegistry := 0
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			switch f.Type {
			case registryT:
				gotRegistry++
			case connMapT:
				t.Errorf("%s.%s is a bare map[string]node.Conn; the node table must live in *nodeRegistry", name, f.Name)
			case mutexPtrT:
				t.Errorf("%s.%s is a *sync.RWMutex; the node-table lock must live in *nodeRegistry", name, f.Name)
			}
		}
		if gotRegistry != wantRegistry {
			t.Errorf("%s has %d *nodeRegistry field(s), want %d", name, gotRegistry, wantRegistry)
		}
	}
	check("Server", reflect.TypeOf(Server{}), 1)
	check("Hub", reflect.TypeOf(Hub{}), 1)
	check("HubOptions", reflect.TypeOf(HubOptions{}), 1)
}
