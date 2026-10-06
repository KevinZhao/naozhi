package workflows

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/naozhi/naozhi/internal/jsonschema"
)

var updateRESTSchema = flag.Bool("update-rest-schema", false, "rewrite testdata/rest.schema.json")

// restResponses are the REST response shapes this package serves, keyed by
// the NZ_CONTRACT.API name of their route. testdata/rest.schema.json is their
// rendering; test/e2e/check-ws-contract.mjs holds the dashboard's typed reads
// to it and scripts/check-mock-rest.test.mjs the e2e mock's responses.
var restResponses = map[string]any{
	"sessions_workflow": WorkflowResponse{},
}

func restSchemaJSON() ([]byte, error) {
	d := jsonschema.New()
	doc := struct {
		Responses map[string]jsonschema.Object `json:"responses"`
		Defs      map[string]jsonschema.Object `json:"defs"`
	}{Responses: map[string]jsonschema.Object{}, Defs: d.Defs}
	for name, v := range restResponses {
		t := reflect.TypeOf(v)
		// Registering the response as a def lets a dashboard function type its
		// parameter with it (@type {WorkflowResponse}).
		def := d.Object(t)
		mode := def.Properties["rows_mode"]
		mode.Enum = []string{RowsFull, RowsNone, RowsDelta}
		def.Properties["rows_mode"] = mode
		d.Defs[t.String()] = def
		doc.Responses[name] = def
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// TestRESTSchema_IsGenerated: the committed schema is exactly what the response
// types render, so a renamed field fails here until the schema is regenerated
// with -update-rest-schema.
func TestRESTSchema_IsGenerated(t *testing.T) {
	want, err := restSchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "rest.schema.json")
	if *updateRESTSchema {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("testdata/rest.schema.json is stale: run `go test ./internal/dashboard/ext/workflows -run TestRESTSchema -update-rest-schema`")
	}
}
