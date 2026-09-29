package wsproto

import (
	"encoding/json"
	"reflect"
	"sort"

	"github.com/naozhi/naozhi/internal/jsonschema"
)

// SchemaJSON renders wsproto.schema.json from the Frames registry: the type
// enum, each frame's properties and required keys, and under defs every
// struct a frame nests, expanded the same way, so a renamed field anywhere in
// a frame's shape changes the file. gen writes it; TestSchema_IsGenerated
// compares it with the committed copy.
func SchemaJSON() ([]byte, error) {
	d := jsonschema.New()
	out := schemaDoc{Frames: map[string]jsonschema.Object{}, Defs: d.Defs}
	for typ, exemplar := range Frames {
		out.Types = append(out.Types, string(typ))
		out.Frames[string(typ)] = d.Object(reflect.TypeOf(exemplar))
	}
	sort.Strings(out.Types)
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

type schemaDoc struct {
	Types  []string                     `json:"types"`
	Frames map[string]jsonschema.Object `json:"frames"`
	Defs   map[string]jsonschema.Object `json:"defs"`
}
