package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestDoctorHealthPayload_MatchesServerContract decodes the /health contract
// that internal/server's TestHealthResp_CoversDoctorContract checks against the
// real encoding. Strict decoding catches a key doctor no longer reads; the
// non-zero walk catches a field doctor reads under a key the contract lacks,
// which would otherwise turn a finding into a silent skip.
func TestDoctorHealthPayload_MatchesServerContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "server", "testdata", "health_doctor_contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p healthPayload
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("contract has a key healthPayload does not decode: %v", err)
	}
	requireNonZero(t, "healthPayload", reflect.ValueOf(p))
}

func requireNonZero(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		f, at := v.Field(i), path+"."+v.Type().Field(i).Name
		if f.IsZero() {
			t.Errorf("%s (json %q) stays zero: the contract has no such key", at, v.Type().Field(i).Tag.Get("json"))
			continue
		}
		if f.Kind() == reflect.Pointer {
			f = f.Elem()
		}
		if f.Kind() == reflect.Struct {
			requireNonZero(t, at, f)
		}
	}
}
