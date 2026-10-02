package jsonschema

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

type describeBase struct {
	ID string `json:"id"`
}

type describeNode struct {
	describeBase
	Name     string          `json:"name,omitempty"`
	Cost     float64         `json:"cost"`
	Raw      json.RawMessage `json:"raw,omitempty"`
	Children []describeNode  `json:"children,omitempty"`
	Skip     string          `json:"-"`
	hidden   string
}

// The describer flattens embedded structs like encoding/json, stops at a
// struct that refers to itself, and names JSON-only shapes by what they carry.
func TestDescriber(t *testing.T) {
	t.Parallel()
	d := New()
	got := d.Object(reflect.TypeOf(describeNode{}))
	keys := make([]string, 0, len(got.Properties))
	for k := range got.Properties {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"children", "cost", "id", "name", "raw"}; !slices.Equal(keys, want) {
		t.Fatalf("properties = %v, want %v (embedded id flattened, json:\"-\" and unexported dropped)", keys, want)
	}
	if want := []string{"cost", "id"}; !slices.Equal(got.Required, want) {
		t.Errorf("required = %v, want %v", got.Required, want)
	}
	if got.Properties["cost"].Type != "number" || got.Properties["raw"].Type != "any" {
		t.Errorf("cost = %+v, raw = %+v", got.Properties["cost"], got.Properties["raw"])
	}
	ch := got.Properties["children"]
	if ch.Type != "array" || ch.Items == nil || ch.Items.Ref != "jsonschema.describeNode" {
		t.Errorf("children = %+v", ch)
	}
	if _, ok := d.Defs["jsonschema.describeNode"].Properties["children"]; !ok {
		t.Errorf("the self-referencing struct's def was not filled: %+v", d.Defs["jsonschema.describeNode"])
	}
	_ = describeNode{}.hidden
	_ = describeNode{}.Skip
}

// Enum is the one key the describer never fills: it is absent until the
// schema's owner sets it, and then renders as JSON-schema's "enum".
func TestProperty_Enum(t *testing.T) {
	t.Parallel()
	plain, err := json.Marshal(Property{Type: "string"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(plain); got != `{"type":"string"}` {
		t.Errorf("an unset enum rendered: %s", got)
	}
	closed, err := json.Marshal(Property{Type: "string", Enum: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(closed); got != `{"type":"string","enum":["a","b"]}` {
		t.Errorf("enum = %s", got)
	}
	if p := New().Object(reflect.TypeOf(describeBase{})).Properties["id"]; p.Enum != nil {
		t.Errorf("the describer filled an enum: %+v", p)
	}
}
