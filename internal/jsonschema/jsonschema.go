// Package jsonschema renders Go types as the small JSON-schema shape the
// contract checks read: per object its properties (type, $ref to a nested
// struct, array items) and required keys, with nested structs expanded once
// under defs. wsproto.schema.json (WS frames) and the REST response schemas
// are both built with it, so the dashboard checks read one format.
package jsonschema

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// Property describes one JSON value.
type Property struct {
	Type string `json:"type"`
	// Ref names the defs entry describing a nested struct.
	Ref string `json:"$ref,omitempty"`
	// Items describes array elements.
	Items *Property `json:"items,omitempty"`
	// Enum, when set, is the closed set of values a string may take. The
	// describer never fills it (a Go string type does not know its values);
	// the schema's owner sets it after describing, as wsproto does for
	// EventEntry.type.
	Enum []string `json:"enum,omitempty"`
}

// Object describes a JSON object: its properties and the keys always present.
type Object struct {
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required"`
}

// Describer renders Go types as JSON object schemas. Every struct a described
// type nests is expanded once under Defs and referenced by its Go type name.
type Describer struct {
	Defs map[string]Object
}

// New returns a Describer with no defs yet.
func New() Describer { return Describer{Defs: map[string]Object{}} }

// Object describes struct type t.
func (d Describer) Object(t reflect.Type) Object {
	os := Object{Properties: map[string]Property{}, Required: []string{}}
	d.fields(t, &os)
	sort.Strings(os.Required)
	return os
}

// fields adds t's JSON fields to os, flattening embedded structs the way
// encoding/json does.
func (d Describer) fields(t reflect.Type, os *Object) {
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

func (d Describer) field(t reflect.Type) Property {
	if t == rawMessage {
		return Property{Type: "any"}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return d.field(t.Elem())
	case reflect.String:
		return Property{Type: "string"}
	case reflect.Bool:
		return Property{Type: "boolean"}
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Uint, reflect.Uint64, reflect.Uint32:
		return Property{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return Property{Type: "number"}
	case reflect.Slice, reflect.Array:
		el := d.field(t.Elem())
		return Property{Type: "array", Items: &el}
	case reflect.Map:
		return Property{Type: "object"}
	case reflect.Interface:
		return Property{Type: "any"}
	case reflect.Struct:
		name := t.String()
		if _, seen := d.Defs[name]; !seen {
			d.Defs[name] = Object{} // placeholder: a self-referencing struct stops here
			d.Defs[name] = d.Object(t)
		}
		return Property{Type: "object", Ref: name}
	}
	return Property{Type: t.Kind().String()}
}
