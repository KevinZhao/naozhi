package session

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
// rendering; scripts/check-rest-contract.mjs holds the dashboard's typed reads
// and the e2e mock's responses to it (#2909).
var restResponses = map[string]any{
	"sessions":         sessionListLocalResp{},
	"sessions_multi":   sessionListMultiResp{},
	"sessions_history": historyListResp{},
}

func restSchemaJSON() ([]byte, error) {
	d := jsonschema.New()
	doc := struct {
		Responses map[string]jsonschema.Object `json:"responses"`
		Defs      map[string]jsonschema.Object `json:"defs"`
	}{Responses: map[string]jsonschema.Object{}, Defs: d.Defs}
	for name, v := range restResponses {
		doc.Responses[name] = d.Object(reflect.TypeOf(v))
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// TestRESTSchema_IsGenerated: the committed schema is exactly what the response
// types render, so a renamed field in SessionSnapshot fails here until the
// schema is regenerated with -update-rest-schema.
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
		t.Error("testdata/rest.schema.json is stale: run `go test ./internal/dashboard/session -run TestRESTSchema -update-rest-schema`")
	}
}
