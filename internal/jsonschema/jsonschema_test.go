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
