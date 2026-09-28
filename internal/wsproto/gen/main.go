// Command gen writes wsproto.schema.json (wsproto.SchemaJSON). Run via
// `go generate ./internal/wsproto`.
package main

import (
	"fmt"
	"os"

	"github.com/naozhi/naozhi/internal/wsproto"
)

func main() {
	data, err := wsproto.SchemaJSON()
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("wsproto.schema.json", data, 0o644); err != nil {
		panic(err)
	}
	fmt.Println("wrote wsproto.schema.json")
}
