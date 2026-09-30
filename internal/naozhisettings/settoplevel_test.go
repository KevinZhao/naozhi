package naozhisettings

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestSetTopLevel_ReplacesInPlaceAndAppendsSorted pins the property callers rely
// on: they own only the keys they pass, and the rest of a hand-edited settings
// file is byte-identical afterwards.
func TestSetTopLevel_ReplacesInPlaceAndAppendsSorted(t *testing.T) {
	doc := []byte(`{
  "env": {"A": "1"},
  "model": "old",
  "outputStyle": "Concise"
}`)
	out, err := SetTopLevel(doc, map[string]json.RawMessage{
		"model":           json.RawMessage(`"new"`),
		"zeta":            json.RawMessage(`1`),
		"availableModels": json.RawMessage(`["a"]`),
	})
	if err != nil {
		t.Fatalf("SetTopLevel: %v", err)
	}
	want := []string{"env", "model", "outputStyle", "availableModels", "zeta"}
	if got := keyOrder(t, out); !reflect.DeepEqual(got, want) {
		t.Errorf("key order = %v, want %v", got, want)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, out)
	}
	if string(parsed["model"]) != `"new"` {
		t.Errorf("model = %s", parsed["model"])
	}
	if !strings.Contains(string(parsed["env"]), `"A"`) {
		t.Errorf("untouched key altered: env = %s", parsed["env"])
	}
}

// TestSetTopLevel_NilDeletes is what lets a probe strip fallbackModel.
func TestSetTopLevel_NilDeletes(t *testing.T) {
	doc := []byte(`{"fallbackModel": ["claude-sonnet-5"], "model": "keep"}`)
	out, err := SetTopLevel(doc, map[string]json.RawMessage{"fallbackModel": nil})
	if err != nil {
		t.Fatalf("SetTopLevel: %v", err)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, out)
	}
	if _, ok := parsed["fallbackModel"]; ok {
		t.Errorf("fallbackModel survived: %s", out)
	}
	if string(parsed["model"]) != `"keep"` {
		t.Errorf("model = %s", parsed["model"])
	}
}

func TestSetTopLevel_DeletingAnAbsentKeyIsANoOp(t *testing.T) {
	out, err := SetTopLevel([]byte(`{"model":"keep"}`), map[string]json.RawMessage{"nope": nil})
	if err != nil {
		t.Fatalf("SetTopLevel: %v", err)
	}
	if got := keyOrder(t, out); !reflect.DeepEqual(got, []string{"model"}) {
		t.Errorf("keys = %v, want just model", got)
	}
}

func TestSetTopLevel_EmptyDocStartsFromAnObject(t *testing.T) {
	for _, doc := range []string{"", "   ", "\n"} {
		out, err := SetTopLevel([]byte(doc), map[string]json.RawMessage{"model": json.RawMessage(`"x"`)})
		if err != nil {
			t.Fatalf("SetTopLevel(%q): %v", doc, err)
		}
		var parsed map[string]string
		if err := json.Unmarshal(out, &parsed); err != nil || parsed["model"] != "x" {
			t.Errorf("SetTopLevel(%q) = %s (err %v)", doc, out, err)
		}
	}
}

func TestSetTopLevel_RejectsNonObject(t *testing.T) {
	if _, err := SetTopLevel([]byte(`[1]`), nil); err == nil {
		t.Fatal("want an error for a non-object document")
	}
}

func keyOrder(t *testing.T, doc []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(doc)))
	if _, err := dec.Token(); err != nil {
		t.Fatalf("read opening token: %v", err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		keys = append(keys, tok.(string))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			t.Fatalf("read value: %v", err)
		}
	}
	return keys
}
