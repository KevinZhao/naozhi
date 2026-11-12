package main

import (
	"bytes"
	"strings"
	"testing"
)

// `naozhi config token` prints a fresh 43-character token each run.
func TestConfigToken(t *testing.T) {
	var a, b bytes.Buffer
	if configToken(&a) != 0 || configToken(&b) != 0 {
		t.Fatal("configToken failed")
	}
	ta, tb := strings.TrimSpace(a.String()), strings.TrimSpace(b.String())
	if len(ta) != 43 || ta == tb {
		t.Errorf("tokens %q / %q: want two distinct 43-char tokens", ta, tb)
	}
}
