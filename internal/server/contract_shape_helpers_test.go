package server

import (
	"reflect"
	"strings"
)

// contractFields splits a wire struct's json tags into required (no
// omitempty) and optional, inlining anonymous embeds the way encoding/json
// flattens them. The shape tests derive their expected key sets from the
// SAME structs the contract.js generator reflects over (#2539), so a tag
// edit updates test expectation, generated contract and wire together — the
// tests then only catch marshal-path divergence, not list rot.
func contractFields(v any) (required, optional []string) {
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(f.Type)
				continue
			}
			tag := f.Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			if strings.Contains(opts, "omitempty") {
				optional = append(optional, name)
			} else {
				required = append(required, name)
			}
		}
	}
	walk(reflect.TypeOf(v))
	return required, optional
}
