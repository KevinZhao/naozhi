package wsproto

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/jsonschema"
)

// SchemaJSON renders wsproto.schema.json from the Frames registry: the type
// enum, each frame's properties and required keys, and under defs every
// struct a frame nests, expanded the same way, so a renamed field anywhere in
// a frame's shape changes the file. gen writes it; TestSchema_IsGenerated
// compares it with the committed copy. The EventEntry def is the wire view
// (wireEventEntry).
func SchemaJSON() ([]byte, error) {
	d := jsonschema.New()
	out := schemaDoc{Frames: map[string]jsonschema.Object{}, Defs: d.Defs}
	for typ, exemplar := range Frames {
		out.Types = append(out.Types, string(typ))
		out.Frames[string(typ)] = d.Object(reflect.TypeOf(exemplar))
	}
	sort.Strings(out.Types)
	if err := wireEventEntry(d.Defs); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// wireEventEntry turns the described EventEntry def into its wire view: type
// is closed over the kind registry (clievent.AllKinds) and the fields ForWire
// clears (clievent.WireOmittedFields) are gone, so a dashboard read of one
// fails tsc against wire.d.ts. A name it does not find is an error, so a
// renamed json tag cannot leave the def describing the Go struct. A todo's
// detail is the CLI's TodoWrite bytes; TodoItem only parses them, so no def.
func wireEventEntry(defs map[string]jsonschema.Object) error {
	const name = "clievent.EventEntry"
	def, ok := defs[name]
	if !ok {
		return fmt.Errorf("no %s def: no frame nests an EventEntry", name)
	}
	typ, ok := def.Properties["type"]
	if !ok {
		return fmt.Errorf("%s has no type property", name)
	}
	typ.Enum = clievent.AllKinds()
	def.Properties["type"] = typ
	for _, k := range clievent.WireOmittedFields() {
		if _, ok := def.Properties[k]; !ok {
			return fmt.Errorf("%s has no %s property to omit", name, k)
		}
		delete(def.Properties, k)
	}
	return nil
}

type schemaDoc struct {
	Types  []string                     `json:"types"`
	Frames map[string]jsonschema.Object `json:"frames"`
	Defs   map[string]jsonschema.Object `json:"defs"`
}
