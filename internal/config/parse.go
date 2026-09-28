package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Raw shapes below use pointers so the parser can tell "field absent"
// (KindMissingField) apart from "field present but illegal"
// (KindInvalidValue).

type rawConfig struct {
	Version   *string                `json:"version"`
	Flags     map[string]rawFlag     `json:"flags"`
	Audiences map[string]rawAudience `json:"audiences"`
}

type rawFlag struct {
	Enabled          *bool    `json:"enabled"`
	Percentage       *int     `json:"percentage"`
	IncludeAudiences []string `json:"include_audiences"`
	ExcludeAudiences []string `json:"exclude_audiences"`
	DependsOn        []string `json:"depends_on"`
	ConflictsWith    []string `json:"conflicts_with"`
}

type rawAudience struct {
	Conditions []rawCondition `json:"conditions"`
}

type rawCondition struct {
	Attribute string   `json:"attribute"`
	Operator  string   `json:"operator"`
	Values    []string `json:"values"`
}

// allowedOperators is the closed set of condition operators.
var allowedOperators = map[string]bool{
	"eq": true, "neq": true, "in": true, "not_in": true,
}

// Parse decodes, normalizes and validates a raw JSON configuration
// document. On any failure it returns a classified *Error and a nil
// config, so callers never observe a partially parsed result: the whole
// document is either accepted or rejected.
func Parse(data []byte) (*Config, error) {
	var raw rawConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, classifyJSONError(err)
	}
	// Reject trailing data after the top-level object.
	if dec.More() {
		return nil, syntaxError("unexpected trailing JSON tokens")
	}

	cfg := &Config{
		Flags:     map[string]*Flag{},
		Audiences: map[string]*Audience{},
	}

	if raw.Version == nil || strings.TrimSpace(*raw.Version) == "" {
		return nil, missingFieldError("version")
	}
	cfg.Version = strings.TrimSpace(*raw.Version)

	if raw.Flags == nil {
		return nil, missingFieldError("flags")
	}

	for key, rf := range raw.Flags {
		if strings.TrimSpace(key) == "" {
			return nil, missingFieldError("flags")
		}
		f := &Flag{
			Key:              key,
			IncludeAudiences: dedupe(rf.IncludeAudiences),
			ExcludeAudiences: dedupe(rf.ExcludeAudiences),
			DependsOn:        dedupe(rf.DependsOn),
			ConflictsWith:    dedupe(rf.ConflictsWith),
		}
		if rf.Enabled != nil {
			f.Enabled = *rf.Enabled
		}
		pctPath := fmt.Sprintf("flags.%s.percentage", key)
		switch {
		case rf.Percentage == nil:
			return nil, missingFieldError(pctPath)
		case *rf.Percentage < 0 || *rf.Percentage > 100:
			return nil, invalidValueError(pctPath,
				fmt.Sprintf("must be in [0,100], got %d", *rf.Percentage))
		default:
			f.Percentage = *rf.Percentage
		}
		cfg.Flags[key] = f
	}

	for name, ra := range raw.Audiences {
		if strings.TrimSpace(name) == "" {
			return nil, missingFieldError("audiences")
		}
		a := &Audience{Name: name}
		if len(ra.Conditions) == 0 {
			return nil, missingFieldError(fmt.Sprintf("audiences.%s.conditions", name))
		}
		for i, rc := range ra.Conditions {
			cPath := fmt.Sprintf("audiences.%s.conditions[%d]", name, i)
			if strings.TrimSpace(rc.Attribute) == "" {
				return nil, missingFieldError(cPath + ".attribute")
			}
			if strings.TrimSpace(rc.Operator) == "" {
				return nil, missingFieldError(cPath + ".operator")
			}
			if !allowedOperators[rc.Operator] {
				return nil, invalidValueError(cPath+".operator",
					fmt.Sprintf("unknown operator %q (allowed: eq, neq, in, not_in)", rc.Operator))
			}
			if len(rc.Values) == 0 {
				return nil, missingFieldError(cPath + ".values")
			}
			for _, v := range rc.Values {
				if v == "" {
					return nil, invalidValueError(cPath+".values", "values must not contain empty strings")
				}
			}
			a.Conditions = append(a.Conditions, Condition{
				Attribute: rc.Attribute,
				Operator:  rc.Operator,
				Values:    append([]string(nil), rc.Values...),
			})
		}
		cfg.Audiences[name] = a
	}

	if err := Validate(cfg); err != nil {
		return nil, err
	}
	cfg.Fingerprint = fingerprint(cfg)
	return cfg, nil
}

func classifyJSONError(err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return syntaxError("invalid JSON at offset %d: %v", se.Offset, err)
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		path := te.Field
		if path == "" {
			path = "(root)"
		}
		return invalidValueError(path, fmt.Sprintf("wrong type: %v", err))
	}
	if strings.HasPrefix(err.Error(), "json: unknown field") {
		return syntaxError("%v", err)
	}
	return syntaxError("%v", err)
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// canonical is the version-independent content used for fingerprints,
// so identical semantics share a fingerprint even with different labels.
type canonical struct {
	Flags     map[string]canonFlag     `json:"flags"`
	Audiences map[string]canonAudience `json:"audiences"`
}

type canonFlag struct {
	Enabled          bool     `json:"enabled"`
	Percentage       int      `json:"percentage"`
	IncludeAudiences []string `json:"include_audiences"`
	ExcludeAudiences []string `json:"exclude_audiences"`
	DependsOn        []string `json:"depends_on"`
	ConflictsWith    []string `json:"conflicts_with"`
}

type canonAudience struct {
	Conditions []Condition `json:"conditions"`
}

func fingerprint(cfg *Config) string {
	c := canonical{
		Flags:     map[string]canonFlag{},
		Audiences: map[string]canonAudience{},
	}
	for k, f := range cfg.Flags {
		c.Flags[k] = canonFlag{
			Enabled:          f.Enabled,
			Percentage:       f.Percentage,
			IncludeAudiences: orEmpty(f.IncludeAudiences),
			ExcludeAudiences: orEmpty(f.ExcludeAudiences),
			DependsOn:        orEmpty(f.DependsOn),
			ConflictsWith:    orEmpty(f.ConflictsWith),
		}
	}
	for k, a := range cfg.Audiences {
		c.Audiences[k] = canonAudience{Conditions: a.Conditions}
	}
	b, err := json.Marshal(c) // map keys are emitted in sorted order
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
