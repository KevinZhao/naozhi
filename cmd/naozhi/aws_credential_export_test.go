package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The export resolves the named profile from the AWS config files, even with
// other credentials in the environment the CLI passes down, and prints them
// in the {"Credentials":{...}} shape the Claude CLI reads.
func TestExportAWSProfileCredentials(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cred.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
echo '{"Version":1,"AccessKeyId":"FIXTURE-AKID-ISO","SecretAccessKey":"FIXTURE-SECRET","SessionToken":"FIXTURE-TOKEN","Expiration":"2030-01-02T03:04:05Z"}'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(dir, "config")
	if err := os.WriteFile(cfgFile, []byte("[profile team-iso]\ncredential_process = "+script+"\n[profile other]\ncredential_process = /bin/false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfgFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials-absent"))
	t.Setenv("AWS_ACCESS_KEY_ID", "FIXTURE-AKID-GLOBAL")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "FIXTURE-SECRET-GLOBAL")
	t.Setenv("AWS_PROFILE", "other")

	out, err := exportAWSProfileCredentials(context.Background(), "team-iso")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var got struct {
		Credentials map[string]string `json:"Credentials"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	want := map[string]string{
		"AccessKeyId":     "FIXTURE-AKID-ISO",
		"SecretAccessKey": "FIXTURE-SECRET",
		"SessionToken":    "FIXTURE-TOKEN",
		"Expiration":      "2030-01-02T03:04:05Z",
	}
	for k, v := range want {
		if got.Credentials[k] != v {
			t.Errorf("Credentials.%s = %q, want %q", k, got.Credentials[k], v)
		}
	}

	if _, err := exportAWSProfileCredentials(context.Background(), "missing"); err == nil {
		t.Error("export of a profile the config lacks succeeded")
	}
	if _, err := exportAWSProfileCredentials(context.Background(), "x;id"); err == nil {
		t.Error("export accepted an unsafe profile name")
	}
}
