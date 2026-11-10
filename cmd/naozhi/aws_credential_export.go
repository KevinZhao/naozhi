package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/naozhi/naozhi/internal/envpolicy"
)

// awsCredentialExportTimeout bounds one credential mint: a credential_process
// (ada) or SSO refresh may hit the network.
const awsCredentialExportTimeout = 60 * time.Second

// runAWSCredentialExport is `naozhi aws-credential-export --profile <name>`,
// the awsCredentialExport an access profile with aws_profile writes into its
// sessions' settings files (docs/rfc/project-access-profile.md §3.1). It
// prints the profile's credentials as {"Credentials":{...}}, the shape the
// Claude CLI reads; stdout carries a live secret, so errors never echo it.
func runAWSCredentialExport(args []string) {
	fs := newFlagSet("aws-credential-export")
	profile := fs.String("profile", "", "AWS profile (in ~/.aws/config) to export credentials for")
	_ = fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), awsCredentialExportTimeout)
	defer cancel()
	out, err := exportAWSProfileCredentials(ctx, *profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "naozhi aws-credential-export:", err)
		os.Exit(1)
	}
	_, _ = os.Stdout.Write(out)
}

// exportAWSProfileCredentials resolves profile through the shared config
// files alone: a profile set programmatically outranks AWS_ACCESS_KEY_ID /
// AWS_PROFILE in the environment, so the global credentials the CLI inherited
// cannot stand in for the named account.
func exportAWSProfileCredentials(ctx context.Context, profile string) ([]byte, error) {
	if !envpolicy.IsSafeProfileValue(profile) {
		return nil, fmt.Errorf("--profile %q is not a valid AWS profile name", profile)
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithSharedConfigProfile(profile))
	if err != nil {
		return nil, fmt.Errorf("load AWS profile %q: %w", profile, err)
	}
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("AWS profile %q yields no credentials", profile)
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("retrieve credentials for AWS profile %q: %w", profile, err)
	}
	type credentials struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		SessionToken    string `json:"SessionToken,omitempty"`
		Expiration      string `json:"Expiration,omitempty"`
	}
	c := credentials{
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
	}
	if creds.CanExpire {
		c.Expiration = creds.Expires.UTC().Format(time.RFC3339)
	}
	out, err := json.Marshal(struct {
		Credentials credentials `json:"Credentials"`
	}{c})
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// credentialExportExe is this binary's absolute path, which access profiles
// with aws_profile run as their awsCredentialExport; "" when it cannot be
// told, which fails those profiles' spawns rather than their accounts.
func credentialExportExe() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if abs, err := filepath.Abs(exe); err == nil {
		return abs
	}
	return ""
}
