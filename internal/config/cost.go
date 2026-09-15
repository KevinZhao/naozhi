package config

// CostConfig tunes the cost ledger (docs/rfc/cost-ledger.md §9). Enabled
// defaults to true; the ledger lives beside session.store_path, so it is also
// off when that is empty. Out-of-range day counts are clamped by the ledger.
type CostConfig struct {
	Enabled       *bool `yaml:"enabled,omitempty"`
	RetentionDays int   `yaml:"retention_days,omitempty"`
	RollupDays    int   `yaml:"rollup_days,omitempty"`
}

// IsEnabled resolves the tri-state Enabled flag (nil = true).
func (c CostConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }
