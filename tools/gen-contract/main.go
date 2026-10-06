// Command gen-contract writes internal/server/static/contract.js (#2539) and
// wire.d.ts (#3439) from the contractjs builders. Run from the repo root:
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
		fail(err)
	}
	write("internal/server/static/contract.js", out)

	dts, err := contractjs.BuildWireDTS("internal/wsproto/wsproto.schema.json", "internal/dashboard/session/testdata/rest.schema.json")
	if err != nil {
		fail(err)
	}
	write("internal/server/static/wire.d.ts", dts)
}

func write(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fail(err)
	}
	fmt.Println("wrote", path)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gen-contract:", err)
	os.Exit(1)
}
