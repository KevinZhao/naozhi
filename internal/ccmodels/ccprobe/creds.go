package ccprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// credExportTimeout bounds the wrapper's credential mint, which may hit the
// network or block on an expired SSO session.
const credExportTimeout = 60 * time.Second

// ExportCredentials runs the snapshot's awsCredentialExport command line and
// returns the credentials it prints. The command is the wrapper's own, so the
// probe authenticates exactly as cc would.
//
// The returned error never contains the command's stdout: that stream carries a
// live secret key.
func ExportCredentials(ctx context.Context, cmdline string) (aws.Credentials, error) {
	argv, err := splitQuoted(cmdline)
	if err != nil {
		return aws.Credentials{}, err
	}
	if len(argv) == 0 {
		return aws.Credentials{}, fmt.Errorf("credential export command is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, credExportTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("run credential export %q: %w", argv[0], err)
	}
	return parseCredentials(out)
}

// parseCredentials reads the wrapper's {"Credentials":{...}} envelope. The
// wrapper may print progress lines first, so the first line that opens an object
// is used.
func parseCredentials(out []byte) (aws.Credentials, error) {
	var env struct {
		Credentials struct {
			AccessKeyID     string `json:"AccessKeyId"`
			SecretAccessKey string `json:"SecretAccessKey"`
			SessionToken    string `json:"SessionToken"`
			Expiration      string `json:"Expiration"`
		} `json:"Credentials"`
	}
	var found bool
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if json.Unmarshal([]byte(line), &env) == nil && env.Credentials.AccessKeyID != "" {
			found = true
			break
		}
	}
	if !found {
		return aws.Credentials{}, fmt.Errorf("credential export printed no usable credentials")
	}
	creds := aws.Credentials{
		AccessKeyID:     env.Credentials.AccessKeyID,
		SecretAccessKey: env.Credentials.SecretAccessKey,
		SessionToken:    env.Credentials.SessionToken,
	}
	if t, err := time.Parse(time.RFC3339, env.Credentials.Expiration); err == nil {
		creds.Expires = t
		creds.CanExpire = true
	}
	return creds, nil
}

// splitQuoted splits a command line on whitespace, honouring double quotes so a
// path containing spaces survives. Single quotes, escapes, and expansion are not
// supported: the wrapper writes a plain quoted path.
func splitQuoted(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	inQuote, hasToken := false, false
	flush := func() {
		if hasToken {
			argv = append(argv, cur.String())
			cur.Reset()
			hasToken = false
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			hasToken = true
		case !inQuote && (r == ' ' || r == '\t'):
			flush()
		default:
			cur.WriteRune(r)
			hasToken = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote in credential export command")
	}
	flush()
	return argv, nil
}
