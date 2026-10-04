package transcribe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolateAWSEnv points every AWS credential source at nothing so the chain
// sees only what the test sets.
func isolateAWSEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, k := range []string{
		"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"AWS_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_SESSION_TOKEN", "AWS_ROLE_ARN",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	} {
		t.Setenv(k, "")
	}
}

func TestCheckCredentials_EnvCredentials(t *testing.T) {
	isolateAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-not-real")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	src, err := CheckCredentials(ctx, "")
	if err != nil {
		t.Fatalf("CheckCredentials: %v", err)
	}
	if src != "EnvConfigCredentials" {
		t.Errorf("source = %q, want EnvConfigCredentials", src)
	}
	if strings.Contains(src, "AKIDEXAMPLE") || strings.Contains(src, "secret-not-real") {
		t.Errorf("source %q leaks key material", src)
	}
}

func TestCheckCredentials_NoCredentials(t *testing.T) {
	isolateAWSEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	src, err := CheckCredentials(ctx, "eu-west-1")
	if err == nil {
		t.Fatalf("CheckCredentials = %q, nil; want an error with no credential source", src)
	}
	if src != "" {
		t.Errorf("source = %q on error, want empty", src)
	}
}

func TestFFmpegPath_Override(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ffmpegPathEnv, bin)
	if got, err := FFmpegPath(); err != nil || got != bin {
		t.Errorf("FFmpegPath() = %q, %v; want %q", got, err, bin)
	}
	t.Setenv(ffmpegPathEnv, filepath.Join(t.TempDir(), "missing"))
	if got, err := FFmpegPath(); err == nil {
		t.Errorf("FFmpegPath() = %q, nil for a missing override; want an error", got)
	}
}
