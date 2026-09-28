package wsproto

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// SchemaJSON renders wsproto.schema.json from the Frames registry: the type
// enum, each frame's properties and required keys, and under defs every
// struct a frame nests, expanded the same way, so a renamed field anywhere in
// a frame's shape changes the file. gen writes it; TestSchema_IsGenerated
// compares it with the committed copy.
func SchemaJSON() ([]byte, error) {
	d := describer{defs: map[string]objectSchema{}}
	out := schemaDoc{Frames: map[string]objectSchema{}, Defs: d.defs}
	for typ, exemplar := range Frames {
		out.Types = append(out.Types, string(typ))
		out.Frames[string(typ)] = d.object(reflect.TypeOf(exemplar))
	}
	sort.Strings(out.Types)
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

type property struct {
	Type string `json:"type"`
	// Ref names the defs entry describing a nested struct.
	Ref string `json:"$ref,omitempty"`
	// Items describes array elements.
	Items *property `json:"items,omitempty"`
}

type objectSchema struct {
	Properties map[string]property `json:"properties"`
	Required   []string            `json:"required"`
}

type schemaDoc struct {
	Types  []string                `json:"types"`
	Frames map[string]objectSchema `json:"frames"`
	Defs   map[string]objectSchema `json:"defs"`
}

type describer struct {
	defs map[string]objectSchema
}

func (d describer) object(t reflect.Type) objectSchema {
	os := objectSchema{Properties: map[string]property{}, Required: []string{}}
	d.fields(t, &os)
	sort.Strings(os.Required)
	return os
}

// fields adds t's JSON fields to os, flattening embedded structs the way
// encoding/json does.
func (d describer) fields(t reflect.Type, os *objectSchema) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || !f.IsExported() && !f.Anonymous {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				d.fields(ft, os)
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		os.Properties[name] = d.field(f.Type)
		if !strings.Contains(opts, "omitempty") {
			os.Required = append(os.Required, name)
		}
	}
}

var rawMessage = reflect.TypeOf(json.RawMessage{})

func (d describer) field(t reflect.Type) property {
	if t == rawMessage {
		return property{Type: "any"}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return d.field(t.Elem())
	case reflect.String:
		return property{Type: "string"}
	case reflect.Bool:
		return property{Type: "boolean"}
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Uint, reflect.Uint64, reflect.Uint32:
		return property{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return property{Type: "number"}
	case reflect.Slice, reflect.Array:
		el := d.field(t.Elem())
		return property{Type: "array", Items: &el}
	case reflect.Map:
		return property{Type: "object"}
	case reflect.Interface:
		return property{Type: "any"}
	case reflect.Struct:
		name := t.String()
		if _, seen := d.defs[name]; !seen {
			d.defs[name] = objectSchema{} // placeholder: a self-referencing struct stops here
			d.defs[name] = d.object(t)
		}
		return property{Type: "object", Ref: name}
	}
	return property{Type: t.Kind().String()}
}
