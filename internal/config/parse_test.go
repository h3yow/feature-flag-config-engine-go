package config

import (
	"strings"
	"testing"
)

func TestParseErrorClassification(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		wantKind ErrorKind
		wantPath string
	}{
		{
			name:     "syntax broken json",
			doc:      `{"version": "v1", "flags": }`,
			wantKind: KindSyntax,
		},
		{
			name:     "syntax unknown field",
			doc:      `{"version":"v1","flags":{},"bogus":1}`,
			wantKind: KindSyntax,
		},
		{
			name: "missing version",
			doc: `{
				"flags": {}
			}`,
			wantKind: KindMissingField,
			wantPath: "version",
		},
		{
			name: "missing flags object",
			doc: `{
				"version": "v1"
			}`,
			wantKind: KindMissingField,
			wantPath: "flags",
		},
		{
			name: "missing percentage",
			doc: `{
				"version": "v1",
				"flags": { "f1": { "enabled": true } }
			}`,
			wantKind: KindMissingField,
			wantPath: "flags.f1.percentage",
		},
		{
			name: "missing audience condition attribute",
			doc: `{
				"version": "v1",
				"flags": {},
				"audiences": {
					"a1": { "conditions": [{ "operator": "eq", "values": ["x"] }] }
				}
			}`,
			wantKind: KindMissingField,
			wantPath: "audiences.a1.conditions[0].attribute",
		},
		{
			name: "illegal percentage high",
			doc: `{
				"version": "v1",
				"flags": { "f1": { "enabled": true, "percentage": 101 } }
			}`,
			wantKind: KindInvalidValue,
			wantPath: "flags.f1.percentage",
		},
		{
			name: "illegal percentage negative",
			doc: `{
				"version": "v1",
				"flags": { "f1": { "enabled": true, "percentage": -1 } }
			}`,
			wantKind: KindInvalidValue,
			wantPath: "flags.f1.percentage",
		},
		{
			name: "illegal operator",
			doc: `{
				"version": "v1",
				"flags": {},
				"audiences": {
					"a1": { "conditions": [{ "attribute": "tier", "operator": "regex", "values": ["x"] }] }
				}
			}`,
			wantKind: KindInvalidValue,
		},
		{
			name: "wrong type",
			doc: `{
				"version": "v1",
				"flags": { "f1": { "enabled": "yes", "percentage": 0 } }
			}`,
			wantKind: KindInvalidValue,
		},
		{
			name: "unknown audience reference",
			doc: `{
				"version": "v1",
				"flags": {
					"f1": { "enabled": true, "percentage": 0, "include_audiences": ["ghost"] }
				}
			}`,
			wantKind: KindReference,
		},
		{
			name: "unknown dependency flag",
			doc: `{
				"version": "v1",
				"flags": {
					"f1": { "enabled": true, "percentage": 0, "depends_on": ["ghost"] }
				}
			}`,
			wantKind: KindReference,
		},
		{
			name: "unknown conflict flag",
			doc: `{
				"version": "v1",
				"flags": {
					"f1": { "enabled": true, "percentage": 0, "conflicts_with": ["ghost"] }
				}
			}`,
			wantKind: KindReference,
		},
		{
			name: "dependency cycle",
			doc: `{
				"version": "v1",
				"flags": {
					"a": { "enabled": true, "percentage": 0, "depends_on": ["b"] },
					"b": { "enabled": true, "percentage": 0, "depends_on": ["c"] },
					"c": { "enabled": true, "percentage": 0, "depends_on": ["a"] }
				}
			}`,
			wantKind: KindCycle,
		},
		{
			name: "self dependency",
			doc: `{
				"version": "v1",
				"flags": {
					"a": { "enabled": true, "percentage": 0, "depends_on": ["a"] }
				}
			}`,
			wantKind: KindInvalidValue,
		},
		{
			name: "asymmetric conflict rejected",
			doc: `{
				"version": "v1",
				"flags": {
					"a": { "enabled": true, "percentage": 0, "conflicts_with": ["b"] },
					"b": { "enabled": true, "percentage": 0 }
				}
			}`,
			wantKind: KindConflict,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Required: log the input config snapshot on every case.
			t.Logf("INPUT SNAPSHOT (%s):\n%s", tc.name, tc.doc)
			cfg, err := Parse([]byte(tc.doc))
			if err == nil {
				t.Fatalf("expected rejection of kind %s, but Parse succeeded: %+v",
					tc.wantKind, cfg)
			}
			ce, ok := AsError(err)
			if !ok {
				t.Fatalf("error is not *config.Error: %T %v", err, err)
			}
			t.Logf("REJECTION: kind=%s path=%s message=%q", ce.Kind, ce.Path, ce.Message)
			if ce.Kind != tc.wantKind {
				t.Fatalf("kind = %s, want %s", ce.Kind, tc.wantKind)
			}
			if tc.wantPath != "" && !strings.HasPrefix(ce.Path, tc.wantPath) {
				t.Fatalf("path = %q, want prefix %q", ce.Path, tc.wantPath)
			}
		})
	}
}

func TestParseValidAndFingerprint(t *testing.T) {
	doc := `{
		"version": "v1",
		"flags": {
			"f1": { "enabled": true, "percentage": 50 }
		},
		"audiences": {}
	}`
	t.Logf("INPUT SNAPSHOT:\n%s", doc)
	cfg, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Version != "v1" || cfg.Fingerprint == "" {
		t.Fatalf("bad parsed config: %+v", cfg)
	}

	// Same content, different formatting and different label: the
	// CONTENT fingerprint must still be equal.
	other := `{"version":"v2-identical-content","flags":{"f1":{"percentage":50,"enabled":true}}}`
	cfg2, err := Parse([]byte(other))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg2.Fingerprint != cfg.Fingerprint {
		t.Fatalf("fingerprints diverged for identical content: %s vs %s",
			cfg.Fingerprint, cfg2.Fingerprint)
	}
}

func TestParseInvalidDocumentLeavesNoPartialResult(t *testing.T) {
	// One bad flag must reject the entire document.
	doc := `{
		"version": "v1",
		"flags": {
			"good": { "enabled": true, "percentage": 10 },
			"bad":  { "enabled": true, "percentage": 999 }
		}
	}`
	cfg, err := Parse([]byte(doc))
	if cfg != nil || err == nil {
		t.Fatalf("expected whole-document rejection, got cfg=%v err=%v", cfg, err)
	}
	if ce, _ := AsError(err); ce.Kind != KindInvalidValue {
		t.Fatalf("want KindInvalidValue, got %s", ce.Kind)
	}
}
