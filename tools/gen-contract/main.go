// Command gen-contract writes internal/server/static/contract.js from the
// contractjs builder (#2539). Run from the repo root:
//
//	go run ./tools/gen-contract
package main

import (
	"fmt"
	"os"

	"github.com/naozhi/naozhi/internal/contractjs"
)

func main() {
	out, err := contractjs.Build("internal/server/testdata/routes.golden.json")
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-contract:", err)
		os.Exit(1)
	}
	if err := os.WriteFile("internal/server/static/contract.js", []byte(out), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen-contract:", err)
		os.Exit(1)
	}
	fmt.Println("wrote internal/server/static/contract.js")
}
