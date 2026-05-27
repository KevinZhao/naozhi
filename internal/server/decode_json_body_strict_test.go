package server

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeJSONBody_RejectsUnknownFields pins #1329
// (R20260527122801-SEC-5): the helper must run with DisallowUnknownFields
// so a future struct field added by an internal patch (e.g. a Privileged
// flag) cannot be mass-assigned by an attacker before the dashboard
// surface deliberately exposes it.
func TestDecodeJSONBody_RejectsUnknownFields(t *testing.T) {
	type req struct {
		Name string `json:"name"`
	}

	body := `{"name":"alice","privileged":true}`
	r := httptest.NewRequest("POST", "/x", strings.NewReader(body))

	var dst req
	err := decodeJSONBody(r, &dst)
	if err == nil {
		t.Fatalf("decodeJSONBody accepted unknown field; want error")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("decodeJSONBody err = %v; want json unknown-field error", err)
	}
	// dst should not have been populated with the legitimate fields either —
	// strict decode bails on the first unknown field, but even if it did, the
	// security property we care about is that the unknown one never lands.
	if dst.Name == "alice" {
		// Acceptable: encoding/json may still set Name before hitting the
		// unknown field. The critical assertion is the error above.
		t.Logf("note: dst.Name populated before unknown-field rejection (allowed)")
	}
}

// TestDecodeJSONBody_AcceptsKnownFields keeps the common path green: a
// well-formed body with exactly the declared fields decodes normally.
func TestDecodeJSONBody_AcceptsKnownFields(t *testing.T) {
	type req struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	body := `{"name":"alice","age":30}`
	r := httptest.NewRequest("POST", "/x", strings.NewReader(body))

	var dst req
	if err := decodeJSONBody(r, &dst); err != nil {
		t.Fatalf("decodeJSONBody rejected valid body: %v", err)
	}
	if dst.Name != "alice" || dst.Age != 30 {
		t.Fatalf("decodeJSONBody didn't populate dst: %+v", dst)
	}
}

// TestDecodeJSONBody_EmptyBody preserves the existing errEmptyJSONBody
// sentinel so callers that errors.Is against it keep working.
func TestDecodeJSONBody_EmptyBody(t *testing.T) {
	r := httptest.NewRequest("POST", "/x", strings.NewReader(""))
	var dst struct{}
	err := decodeJSONBody(r, &dst)
	if !errors.Is(err, errEmptyJSONBody) {
		t.Fatalf("decodeJSONBody empty body err = %v; want errEmptyJSONBody", err)
	}
}

// TestDecodeJSONBody_ClosesBody confirms the helper still calls Close on
// the request body so the existing lifecycle contract holds.
func TestDecodeJSONBody_ClosesBody(t *testing.T) {
	tracker := &closeTrackingReader{Reader: strings.NewReader(`{}`)}
	r := httptest.NewRequest("POST", "/x", tracker)
	var dst struct{}
	if err := decodeJSONBody(r, &dst); err != nil {
		t.Fatalf("decodeJSONBody empty struct decode: %v", err)
	}
	if !tracker.closed {
		t.Fatalf("decodeJSONBody did not close request body")
	}
}

type closeTrackingReader struct {
	io.Reader
	closed bool
}

func (c *closeTrackingReader) Close() error {
	c.closed = true
	return nil
}
