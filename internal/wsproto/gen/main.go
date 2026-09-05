// Command gen emits wsproto.schema.json from the Frames registry: per-type
// properties/required plus the MsgType enum. Zero dependencies — a small
// reflect walk is all the schema's two consumers (the backend contract test
// and test/e2e/check-ws-contract.mjs) need. Run via
// `go generate ./internal/wsproto`.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/naozhi/naozhi/internal/wsproto"
)

type property struct {
	Type string `json:"type"`
	// Ref names a Go type this generator deliberately does not expand:
	// clievent.EventEntry's shape (and versioning) is #2496 C3's territory.
	Ref string `json:"$ref,omitempty"`
	// Items describes array elements ($ref-only, same boundary).
	Items *property `json:"items,omitempty"`
}

type frameSchema struct {
	Properties map[string]property `json:"properties"`
	Required   []string            `json:"required"`
}

type schema struct {
	Types  []string               `json:"types"`
	Frames map[string]frameSchema `json:"frames"`
}

func main() {
	out := schema{Frames: map[string]frameSchema{}}
	for typ, exemplar := range wsproto.Frames {
		out.Types = append(out.Types, string(typ))
		out.Frames[string(typ)] = describe(reflect.TypeOf(exemplar))
	}
	sort.Strings(out.Types)
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("wsproto.schema.json", append(data, '\n'), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote wsproto.schema.json (%d types)\n", len(out.Types))
}

func describe(t reflect.Type) frameSchema {
	fs := frameSchema{Properties: map[string]property{}, Required: []string{}}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		fs.Properties[name] = describeField(f.Type)
		if !strings.Contains(opts, "omitempty") {
			fs.Required = append(fs.Required, name)
		}
	}
	sort.Strings(fs.Required)
	return fs
}

func describeField(t reflect.Type) property {
	switch t.Kind() {
	case reflect.Pointer:
		return describeField(t.Elem())
	case reflect.String:
		return property{Type: "string"}
	case reflect.Bool:
		return property{Type: "boolean"}
	case reflect.Int, reflect.Int64, reflect.Int32:
		return property{Type: "integer"}
	case reflect.Slice:
		el := describeField(t.Elem())
		return property{Type: "array", Items: &el}
	case reflect.Struct:
		// Nested structs stay $ref-only: clievent.EventEntry's field shape
		// (and its versioning) is #2496 C3's territory, not this schema's.
		return property{Type: "object", Ref: t.String()}
	}
	return property{Type: t.Kind().String()}
}
