package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// doctorContractPath holds the /health fields `naozhi doctor` decodes. The
// doctor side of the contract (cmd/naozhi) decodes the same file strictly.
var doctorContractPath = filepath.Join("testdata", "health_doctor_contract.json")

// fillNonZero sets every reachable exported field so omitempty drops nothing
// and the encoding shows every key the struct can emit.
func fillNonZero(v reflect.Value, depth int) {
	if depth > 8 || !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillNonZero(v.Elem(), depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fillNonZero(v.Field(i), depth+1)
		}
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		e := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(k, depth+1)
		fillNonZero(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Interface:
		if v.NumMethod() == 0 {
			v.Set(reflect.ValueOf("x"))
		}
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	}
}

// TestHealthResp_CoversDoctorContract: every key doctor reads must exist in
// the authenticated /health encoding with the same JSON kind. An empty object
// in the contract stands for a map whose keys are data.
func TestHealthResp_CoversDoctorContract(t *testing.T) {
	// The embed is unexported, so the walk cannot allocate it.
	resp := healthResp{healthAuthSection: &healthAuthSection{}}
	fillNonZero(reflect.ValueOf(&resp).Elem(), 0)
	fillNonZero(reflect.ValueOf(resp.healthAuthSection).Elem(), 0)
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var server map[string]any
	if err := json.Unmarshal(raw, &server); err != nil {
		t.Fatal(err)
	}
	contractRaw, err := os.ReadFile(doctorContractPath)
	if err != nil {
		t.Fatal(err)
	}
	var contract map[string]any
	if err := json.Unmarshal(contractRaw, &contract); err != nil {
		t.Fatal(err)
	}
	compareContract(t, "", contract, server)
}

func compareContract(t *testing.T, path string, contract, server map[string]any) {
	t.Helper()
	for k, want := range contract {
		at := path + k
		got, ok := server[k]
		if !ok {
			t.Errorf("/health no longer encodes %s, which naozhi doctor reads", at)
			continue
		}
		if wk, gk := reflect.TypeOf(want), reflect.TypeOf(got); wk != gk {
			t.Errorf("/health %s is JSON %v, naozhi doctor expects %v", at, gk, wk)
			continue
		}
		if sub, ok := want.(map[string]any); ok {
			compareContract(t, at+".", sub, got.(map[string]any))
		}
	}
}
