package config

// ReverseNodeEntry configures a node that is allowed to reverse-connect to this primary.
type ReverseNodeEntry struct {
	Token       string `yaml:"token"`
	DisplayName string `yaml:"display_name"`
}
