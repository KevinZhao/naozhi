package cron

import "fmt"

// Field size / character checks for cron jobs live in one place, the scheduler
// write path (cron.validateJobFields / UpdateJob); this file keeps only the
// dashboard-specific product rules.

// validNotifyPlatforms is the notify_platform allowlist. The dashboard field is
// free text, so a misspelt name would otherwise be stored and its
// notifications silently dropped.
var validNotifyPlatforms = map[string]struct{}{
	"":        {}, // empty = fall back to cron.notify_default
	"feishu":  {},
	"slack":   {},
	"discord": {},
	"weixin":  {},
}

func validateNotifyPlatform(platform string) error {
	if _, ok := validNotifyPlatforms[platform]; !ok {
		return fmt.Errorf("invalid notify_platform")
	}
	return nil
}

// ValidateCronBackend enforces the shape contract for the dashboard-picked
// backend override: empty OK, length <= maxBackendIDLen, charset per
// isValidBackendID (shared with the WS path). Unknown backend IDs are NOT
// rejected — the router's wrapperFor clamps them to the default so a job keeps
// running after an operator removes a backend from config.yaml.
func ValidateCronBackend(backend string) error {
	if backend == "" {
		return nil
	}
	if len(backend) > maxBackendIDLen {
		return fmt.Errorf("backend exceeds %d-byte limit", maxBackendIDLen)
	}
	if !isValidBackendID(backend) {
		// Same error string as send.handleSend's gate.
		return fmt.Errorf("invalid backend identifier")
	}
	return nil
}
