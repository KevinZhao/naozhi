package wsproto_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/jsonschema"
	"github.com/naozhi/naozhi/internal/wsproto"
)

type schemaDoc struct {
	Types  []string `json:"types"`
	Frames map[string]struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	} `json:"frames"`
}

// TestSchema_CoversEveryFrame walks the Frames registry against the
// committed wsproto.schema.json: every type is in the enum, every marshaled
// exemplar key is a declared property, and every required property appears.
// A frame edit without `go generate ./internal/wsproto` fails here.
func TestSchema_CoversEveryFrame(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("wsproto.schema.json")
	if err != nil {
		t.Fatalf("read schema (run `go generate ./internal/wsproto`): %v", err)
	}
	var doc schemaDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	enum := map[string]bool{}
	for _, s := range doc.Types {
		enum[s] = true
	}
	if len(enum) != len(wsproto.Frames) {
		t.Errorf("schema enum has %d types, registry has %d — regenerate", len(enum), len(wsproto.Frames))
	}

	for typ, exemplar := range wsproto.Frames {
		if !enum[string(typ)] {
			t.Errorf("schema enum missing %q — regenerate", typ)
			continue
		}
		fs, ok := doc.Frames[string(typ)]
		if !ok {
			t.Errorf("schema has no frame entry for %q — regenerate", typ)
			continue
		}
		raw, err := json.Marshal(exemplar)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		for key := range got {
			if _, ok := fs.Properties[key]; !ok {
				t.Errorf("%s: marshaled key %q not in schema properties — regenerate", typ, key)
			}
		}
		for _, req := range fs.Required {
			if _, ok := got[req]; !ok {
				t.Errorf("%s: required property %q absent from the exemplar marshal", typ, req)
			}
		}
	}
}

// TestSchema_IsGenerated: the committed schema is exactly what the registry
// renders, nested structs included, so a renamed field in EventEntry or any
// other nested shape fails here until the schema is regenerated — and the
// regenerated file is what check-ws-contract.mjs holds the dashboard to.
func TestSchema_IsGenerated(t *testing.T) {
	t.Parallel()
	want, err := wsproto.SchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("wsproto.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("wsproto.schema.json is stale: run `go generate ./internal/wsproto`")
	}
}

// The committed EventEntry def is the wire view: type is closed over the kind
// registry, and no field ForWire clears is declared, so the dashboard checks
// that read this file hold a read of one to be an error.
func TestSchema_EventEntryIsTheWireView(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("wsproto.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]jsonschema.Object `json:"defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	def, ok := doc.Defs["clievent.EventEntry"]
	if !ok {
		t.Fatal("no clievent.EventEntry def")
	}
	if got := def.Properties["type"].Enum; !slices.Equal(got, clievent.AllKinds()) {
		t.Errorf("type enum = %v, want clievent.AllKinds() %v", got, clievent.AllKinds())
	}
	for _, k := range clievent.WireOmittedFields() {
		if _, ok := def.Properties[k]; ok {
			t.Errorf("the def declares %s, which ForWire clears", k)
		}
	}
}
