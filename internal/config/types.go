package config

// Config is one complete, immutable configuration version.
type Config struct {
	// Version is the user supplied version label (required, unique).
	Version string
	// Flags maps flag key -> flag definition.
	Flags map[string]*Flag
	// Audiences maps audience name -> audience definition.
	Audiences map[string]*Audience

	// Fingerprint is a canonical hash of the parsed content and is
	// recomputed by the parser; two configs with identical semantics
	// share the same fingerprint even if the version label differs.
	Fingerprint string
}

// Flag describes a single feature flag.
type Flag struct {
	Key     string
	Enabled bool
	// Percentage in [0,100]; release only applies when Enabled is true.
	Percentage int
	// Audiences this flag always serves ON to.
	IncludeAudiences []string
	// Audiences this flag always serves OFF to (checked before include).
	ExcludeAudiences []string
	// DependsOn lists flags that must evaluate ON for this flag to be ON.
	DependsOn []string
	// ConflictsWith lists flags that must evaluate OFF for this flag to be ON.
	ConflictsWith []string
}

// Audience is a reusable condition set over user attributes.
type Audience struct {
	Name       string
	Conditions []Condition
}

// Condition matches one user attribute.
type Condition struct {
	Attribute string
	Operator  string
	Values    []string
}
