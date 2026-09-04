// Package cron — redact_secrets.go: thin aliases over textutil.RedactSecrets,
// kept so the in-package call site (scheduler_finish.go) and the exported
// symbol stay stable while consumers migrate (#1571).

package cron

import "github.com/naozhi/naozhi/internal/textutil"

// RedactSecrets scrubs known credential token shapes (sk-ant-, ghp_, AKIA, …)
// from s.
//
// Deprecated: use textutil.RedactSecrets directly. Retained as a thin alias
// for one or two releases (#1571).
func RedactSecrets(s string) string { return textutil.RedactSecrets(s) }

// redactSecretsInResult is the in-package call name used by scheduler_finish.go.
func redactSecretsInResult(s string) string { return textutil.RedactSecrets(s) }
