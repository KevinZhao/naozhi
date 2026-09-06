// rule 3b-send (send_engine_ownership): the send pipeline's state belongs to
// sendEngine, not Hub (#2551, docs/rfc/send-engine-extraction.md §2.3).
//
// This is the send-block slice of the "rule 3b AST field_block 对账" that
// main.go has listed as owed since Phase 4b. Phase 4b was shelved by ADR-001,
// so the general rule was never going to arrive; #2551 delivers the part it can
// actually check today. Rule 3a only verifies that a marker comment EXISTS —
// its content is never validated, which is how wshub_send.go's header ended up
// claiming ownership of a field that no longer exists. These checks look at the
// declarations instead, so they cannot drift into fiction.
//
// Deliberately AST-on-declarations rather than a type-checked search for
// `h.queue` reads: no go/types pass, no build tags to satisfy, and the
// invariant is the same. If Hub cannot declare the field, nothing can read it
// off a Hub.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
)

// sendOwnedFields are the fields that migrated from Hub to sendEngine. Hub must
// not declare any of them; sendEngine must declare all of them.
var sendOwnedFields = []string{"queue", "guard", "wg", "trackMu", "closed", "legacyInvokes"}

// sendPipelineFiles hold the pipeline methods. They must carry *sendEngine
// receivers only: a *Hub method here means a piece of the pipeline was written
// back onto the Hub.
var sendPipelineFiles = []string{"send.go", "send_owner_loop.go", "send_engine.go"}

// scanSendEngineOwnership implements rule 3b-send.
func scanSendEngineOwnership(pkgDir string) []Violation {
	var out []Violation
	fset := token.NewFileSet()

	// A + C: field ownership, read off the two struct declarations.
	hubFields, engineFields := map[string]bool{}, map[string]bool{}
	var hubFile, engineFile string
	for _, name := range []string{"wshub.go", "send_engine.go"} {
		path := filepath.Join(pkgDir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			out = append(out, Violation{
				Rule:    "send_engine_ownership",
				File:    filepath.ToSlash(path),
				Message: fmt.Sprintf("parse failed, cannot check send-field ownership: %v", err),
			})
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			var into map[string]bool
			switch ts.Name.Name {
			case "Hub":
				into, hubFile = hubFields, filepath.ToSlash(path)
			case "sendEngine":
				into, engineFile = engineFields, filepath.ToSlash(path)
			default:
				return true
			}
			for _, fld := range st.Fields.List {
				for _, id := range fld.Names {
					into[id.Name] = true
				}
			}
			return true
		})
	}

	if hubFile == "" {
		out = append(out, Violation{
			Rule: "send_engine_ownership", File: filepath.ToSlash(filepath.Join(pkgDir, "wshub.go")),
			Message: "type Hub not found — rule 3b-send cannot verify that the send block stayed off the Hub",
		})
	}
	if engineFile == "" {
		out = append(out, Violation{
			Rule: "send_engine_ownership", File: filepath.ToSlash(filepath.Join(pkgDir, "send_engine.go")),
			Message: "type sendEngine not found — the send pipeline lost its owner (#2551); if it moved, move this rule with it",
		})
		return out
	}

	for _, name := range sendOwnedFields {
		if hubFields[name] {
			out = append(out, Violation{
				Rule: "send_engine_ownership", File: hubFile,
				Message: fmt.Sprintf("Hub declares %q, which sendEngine owns (#2551). Two copies of the send state means the WS path and the HTTP path can disagree; put it on sendEngine and reach it via h.engine", name),
			})
		}
		if !engineFields[name] {
			out = append(out, Violation{
				Rule: "send_engine_ownership", File: engineFile,
				Message: fmt.Sprintf("sendEngine no longer declares %q. If the field was genuinely removed, drop it from sendOwnedFields in this rule and say why in the commit; if it moved back to Hub, that is the regression #2551 fixed", name),
			})
		}
	}

	// B: the pipeline files must not carry *Hub methods.
	for _, name := range sendPipelineFiles {
		path := filepath.Join(pkgDir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			out = append(out, Violation{
				Rule: "send_engine_ownership", File: filepath.ToSlash(path),
				Message: fmt.Sprintf("parse failed, cannot check receivers: %v", err),
			})
			continue
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			// recvTypeName lives in main.go (shared with rule 1).
			if recvTypeName(fd.Recv.List[0].Type) != "Hub" {
				continue
			}
			out = append(out, Violation{
				Rule:    "send_engine_ownership",
				File:    filepath.ToSlash(path),
				Line:    fset.Position(fd.Pos()).Line,
				Message: fmt.Sprintf("%s has a *Hub receiver in a send-pipeline file (#2551). Pipeline methods take *sendEngine; if this really is Hub's job (a WS protocol adapter, a node-table accessor), put it in the wshub_*.go file for that job — lookupNode sitting here is what made the chain's method count read 13 instead of 12", fd.Name.Name),
			})
		}
	}
	return out
}
