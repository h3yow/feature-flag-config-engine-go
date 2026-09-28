package config

import "sort"

func sortedFlagKeys(cfg *Config) []string {
	keys := make([]string, 0, len(cfg.Flags))
	for k := range cfg.Flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
