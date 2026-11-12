package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/textutil"
)

// configToken is `naozhi config token`: it prints a random dashboard_token
// (32 bytes, base64url, 43 characters) for the operator to put in config.yaml.
// Only stdout carries it, so it stays out of the service log.
func configToken(out io.Writer) int {
	tok, err := newDashboardToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config token: %v\n", err)
		return 1
	}
	fmt.Fprintln(out, tok)
	return 0
}

func newDashboardToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// registerConfigSecrets has the transcript, IM-reply and log scrubbers
// (textutil.RedactSecrets) replace naozhi's own credentials verbatim.
func registerConfigSecrets(cfg *config.Config) {
	secrets := []string{cfg.Server.DashboardToken}
	if cfg.Upstream != nil {
		secrets = append(secrets, cfg.Upstream.Token)
	}
	for _, n := range cfg.ReverseNodes {
		secrets = append(secrets, n.Token)
	}
	for _, n := range cfg.Workspaces {
		secrets = append(secrets, n.Token)
	}
	textutil.RegisterLiteralSecrets(secrets...)
}
