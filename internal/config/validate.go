package config

import "fmt"

// Validate runs all semantic checks on a fully parsed configuration:
// audience/flag references, self references, dependency cycles and
// conflict-graph symmetry. Every failure is a classified *Error and
// nothing is partially applied: callers must reject the whole document.
func Validate(cfg *Config) error {
	if err := validateReferences(cfg); err != nil {
		return err
	}
	if err := validateDependencies(cfg); err != nil {
		return err
	}
	return validateConflicts(cfg)
}

func validateReferences(cfg *Config) error {
	for key, f := range cfg.Flags {
		for _, aud := range f.IncludeAudiences {
			if _, ok := cfg.Audiences[aud]; !ok {
				return referenceError(
					fmt.Sprintf("flags.%s.include_audiences", key),
					fmt.Sprintf("references unknown audience %q", aud))
			}
		}
		for _, aud := range f.ExcludeAudiences {
			if _, ok := cfg.Audiences[aud]; !ok {
				return referenceError(
					fmt.Sprintf("flags.%s.exclude_audiences", key),
					fmt.Sprintf("references unknown audience %q", aud))
			}
		}
		for _, dep := range f.DependsOn {
			if dep == key {
				return invalidValueError(
					fmt.Sprintf("flags.%s.depends_on", key),
					"a flag must not depend on itself")
			}
			if _, ok := cfg.Flags[dep]; !ok {
				return referenceError(
					fmt.Sprintf("flags.%s.depends_on", key),
					fmt.Sprintf("references unknown flag %q", dep))
			}
		}
		for _, other := range f.ConflictsWith {
			if other == key {
				return invalidValueError(
					fmt.Sprintf("flags.%s.conflicts_with", key),
					"a flag must not conflict with itself")
			}
			if _, ok := cfg.Flags[other]; !ok {
				return referenceError(
					fmt.Sprintf("flags.%s.conflicts_with", key),
					fmt.Sprintf("references unknown flag %q", other))
			}
		}
	}
	return nil
}

// validateDependencies detects cycles in the depends_on graph.
func validateDependencies(cfg *Config) error {
	const (
		white = 0 // unvisited
		gray  = 1 // on current DFS stack
		black = 2 // fully explored
	)
	color := map[string]int{}
	var stack []string

	var visit func(key string) []string
	visit = func(key string) []string {
		color[key] = gray
		stack = append(stack, key)
		for _, dep := range cfg.Flags[key].DependsOn {
			switch color[dep] {
			case white:
				if cyc := visit(dep); cyc != nil {
					return cyc
				}
			case gray:
				// Extract the cycle from the current stack.
				for i, k := range stack {
					if k == dep {
						return append(append([]string{}, stack[i:]...), dep)
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[key] = black
		return nil
	}

	// Deterministic traversal keeps error messages stable.
	for _, key := range sortedFlagKeys(cfg) {
		if color[key] == white {
			if cyc := visit(key); cyc != nil {
				return cycleError(cyc)
			}
		}
	}
	return nil
}

// validateConflicts requires conflicts to be declared symmetrically;
// a one-sided declaration is ambiguous and therefore rejected.
func validateConflicts(cfg *Config) error {
	for _, key := range sortedFlagKeys(cfg) {
		f := cfg.Flags[key]
		for _, other := range f.ConflictsWith {
			peer := cfg.Flags[other]
			if !contains(peer.ConflictsWith, key) {
				return conflictError(
					fmt.Sprintf("flags.%s.conflicts_with", key),
					fmt.Sprintf("flag %q declares a conflict with %q but the reverse declaration is missing",
						key, other))
			}
		}
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
