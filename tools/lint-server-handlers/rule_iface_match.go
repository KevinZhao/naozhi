// rule 4 (iface_match): every godoc "satisfies: <pkg>.<Iface>" note must
// have a matching entry in docs/design/server-consumer-contracts.md.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// satisfiesRE matches godoc lines like:
//
//	// satisfies: server.MessageEnqueuer
//	// satisfies-by: *dispatch.MessageQueue (internal/dispatch/msgqueue.go)
//
// Group 1 is the interface ref; trailing parens are provenance only.
var satisfiesRE = regexp.MustCompile(`(?m)^//\s*satisfies(?:-by)?:\s*([\w./*]+)`)

// scanIfaceMatch walks .go files under roots and reports satisfies:
// declarations whose interface name does not appear anywhere in
// consumer-contracts.md (name presence only, not method-set parity).
func scanIfaceMatch(roots []string) []Violation {
	var out []Violation

	contractsPath := "docs/design/server-consumer-contracts.md"
	contractsData, err := os.ReadFile(contractsPath)
	if err != nil {
		// Missing doc is a single warning, not a hard error.
		out = append(out, Violation{
			Rule:    "iface_match",
			File:    contractsPath,
			Message: fmt.Sprintf("consumer-contracts.md not readable (%v); rule 4 cannot validate satisfies declarations", err),
		})
		return out
	}
	contracts := string(contractsData)

	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			matches := satisfiesRE.FindAllStringSubmatch(string(data), -1)
			for _, m := range matches {
				if len(m) < 2 {
					continue
				}
				ifaceRef := strings.TrimSpace(m[1])
				ifaceRef = strings.TrimPrefix(ifaceRef, "*")
				parts := strings.Split(ifaceRef, ".")
				ifaceName := parts[len(parts)-1]
				// Heuristic: contracts.md must mention the interface name somewhere.
				if !strings.Contains(contracts, ifaceName) {
					out = append(out, Violation{
						Rule:    "iface_match",
						File:    filepath.ToSlash(path),
						Message: fmt.Sprintf("godoc declares 'satisfies: %s' but consumer-contracts.md has no entry for %q (Phase 0b stub heuristic; Phase 1 前升级 method-set 完整对账)", m[1], ifaceName),
					})
				}
			}
			return nil
		})
	}
	return out
}
