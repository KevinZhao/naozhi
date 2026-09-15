package config

import "fmt"

// validateNotifyTargets checks that a configured notify platform exists. A typo
// would otherwise only surface when a notification fires — for cron, hours after
// the job ran. Split out of validateConfig (#2710).
func validateNotifyTargets(cfg *Config) error {
	// A notify platform typo would otherwise only surface when a notification
	// actually fires. Empty is legal (disables the default).
	if np := cfg.Cron.NotifyDefault.Platform; np != "" {
		if !cfg.hasPlatform(np) {
			return fmt.Errorf("cron.notify_default.platform %q is not a configured platform (set platforms.%s or clear notify_default)", np, np)
		}
	}
	if np := cfg.Update.Notify.Platform; np != "" {
		if !cfg.hasPlatform(np) {
			return fmt.Errorf("update.notify.platform %q is not a configured platform (set platforms.%s or clear update.notify)", np, np)
		}
	}
	return nil
}
