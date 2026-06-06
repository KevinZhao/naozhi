// Package cron — redact_secrets.go: thin aliases over the shared secret
// scrubber now living in internal/textutil.
//
// History: the token-prefix redactor was first written here (R234-SEC-7,
// #1006) to scrub CronRun.Result / Job.LastResult before persistence + WS
// broadcast. IM dispatch later imported cron.RedactSecrets to scrub Claude
// replies — a security-critical path coupled to a domain-unrelated package.
// R20260602-091302-ARCH-1 (#1571) relocated the scan logic to the leaf
// package internal/textutil so cron and dispatch each import the leaf
// directly. These aliases keep the in-package call sites (scheduler_finish.go)
// and the exported symbol stable; remove a release or two after consumers
// migrate to textutil.RedactSecrets.

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
