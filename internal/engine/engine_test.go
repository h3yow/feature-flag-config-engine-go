package engine

import (
	"fmt"
	"testing"

	"github.com/h3yow/feature-flag-config-engine-go/internal/bucket"
	"github.com/h3yow/feature-flag-config-engine-go/internal/config"
)

func mustEngine(t *testing.T, raw string) *Engine {
	t.Helper()
	e, err := New([]byte(raw))
	if err != nil {
		t.Fatalf("bootstrap failed: %v\ninput:\n%s", err, raw)
	}
	return e
}

func logBasis(t *testing.T, raw string, u User, key string, r Result) {
	t.Helper()
	t.Logf("CONFIG SNAPSHOT:\n%s", raw)
	t.Logf("EVAL INPUT: user=%+v flag=%s", u, key)
	t.Logf("EVAL BASIS: on=%v reason=%s bucket=%d matched_aud=%q related=%q",
		r.On, r.Reason, r.Bucket, r.MatchedAud, r.RelatedFlag)
}

const evalCfg = `{
  "version": "v1",
  "flags": {
    "gate":        { "enabled": true, "percentage": 100 },
    "off_master":  { "enabled": false, "percentage": 100 },
    "needs_gate":  { "enabled": true, "percentage": 100, "depends_on": ["gate"] },
    "needs_off":   { "enabled": true, "percentage": 100, "depends_on": ["off_master"] },
    "rollout":     { "enabled": true, "percentage": 50 },
    "rollout0":    { "enabled": true, "percentage": 0 },
    "rollout100":  { "enabled": true, "percentage": 100 },
    "staff_flag":  { "enabled": true, "percentage": 0, "include_audiences": ["staff"] },
    "blocked_flag":{ "enabled": true, "percentage": 100, "exclude_audiences": ["blocked"] },
    "alpha":       { "enabled": true, "percentage": 100, "conflicts_with": ["beta"] },
    "beta":        { "enabled": true, "percentage": 100, "conflicts_with": ["alpha"] }
  },
  "audiences": {
    "staff":   { "conditions": [{ "attribute": "tier", "operator": "eq", "values": ["staff"] }] },
    "blocked": { "conditions": [{ "attribute": "blocked", "operator": "eq", "values": ["true"] }] }
  }
}`

func TestEvaluateBasicReasons(t *testing.T) {
	e := mustEngine(t, evalCfg)
	u := User{ID: "u-1"}

	check := func(key string, wantOn bool, wantReason Reason) {
		t.Helper()
		r := e.Evaluate(u, key)
		logBasis(t, evalCfg, u, key, r)
		if r.On != wantOn || r.Reason != wantReason {
			t.Fatalf("%s: got on=%v reason=%s, want on=%v reason=%s",
				key, r.On, r.Reason, wantOn, wantReason)
		}
	}

	check("off_master", false, ReasonFlagDisabled)
	check("needs_gate", true, ReasonInRollout)
	check("needs_off", false, ReasonDependencyOff)
	check("rollout0", false, ReasonOutOfRollout)
	check("rollout100", true, ReasonInRollout)
	check("does_not_exist", false, ReasonUnknownFlag)
}

func TestEvaluateAudiences(t *testing.T) {
	e := mustEngine(t, evalCfg)

	staff := User{ID: "u-2", Attributes: map[string]string{"tier": "staff"}}
	r := e.Evaluate(staff, "staff_flag")
	logBasis(t, evalCfg, staff, "staff_flag", r)
	if !r.On || r.Reason != ReasonIncluded || r.MatchedAud != "staff" {
		t.Fatalf("staff include failed: %+v", r)
	}

	blocked := User{ID: "u-3", Attributes: map[string]string{"blocked": "true"}}
	r = e.Evaluate(blocked, "blocked_flag")
	logBasis(t, evalCfg, blocked, "blocked_flag", r)
	if r.On || r.Reason != ReasonExcluded || r.MatchedAud != "blocked" {
		t.Fatalf("exclude failed: %+v", r)
	}
}

func TestConflictDeterministicWinner(t *testing.T) {
	e := mustEngine(t, evalCfg)
	u := User{ID: "u-9"}
	ra := e.Evaluate(u, "alpha")
	rb := e.Evaluate(u, "beta")
	logBasis(t, evalCfg, u, "alpha", ra)
	logBasis(t, evalCfg, u, "beta", rb)
	// Both are at 100%; alpha < beta lexicographically, so alpha wins.
	if !ra.On {
		t.Fatalf("alpha should win the conflict: %+v", ra)
	}
	if rb.On || rb.Reason != ReasonConflictActive || rb.RelatedFlag != "alpha" {
		t.Fatalf("beta must be suppressed by alpha: %+v", rb)
	}
	// Same user + same version => identical results on repeat.
	if e.Evaluate(u, "alpha") != ra {
		t.Fatal("evaluation is not reproducible")
	}
}

func TestRolloutBoundaries(t *testing.T) {
	e := mustEngine(t, evalCfg)
	cfg := e.Snapshot()

	// Find users exactly on bucket 0, 49, 50, 99 for the rollout flag.
	wanted := map[int]string{}
	for i := 0; len(wanted) < 4 && i < 5_000_000; i++ {
		id := fmt.Sprintf("boundary-user-%d", i)
		b := bucket.Bucket("rollout", cfg.Fingerprint, id)
		if _, ok := wanted[b]; !ok && (b == 0 || b == 49 || b == 50 || b == 99) {
			wanted[b] = id
		}
	}
	for _, b := range []int{0, 49, 50, 99} {
		id, ok := wanted[b]
		if !ok {
			t.Fatalf("could not find a user on bucket %d", b)
		}
		u := User{ID: id}
		r := e.Evaluate(u, "rollout")
		logBasis(t, evalCfg, u, "rollout", r)
		if r.Bucket != b {
			t.Fatalf("reported bucket %d != found bucket %d", r.Bucket, b)
		}
		wantOn := b < 50
		if r.On != wantOn {
			t.Fatalf("boundary bucket %d: on=%v want %v (rule: bucket < percentage)",
				b, r.On, wantOn)
		}
	}
}

func TestInvalidUpdateRejectedAndOldVersionStays(t *testing.T) {
	e := mustEngine(t, evalCfg)
	beforeVer := e.ActiveVersion()
	beforeFP := e.ActiveFingerprint()

	invalid := `{"version":"v2-bad","flags":{"f":{"enabled":true,"percentage":500}}}`
	t.Logf("ATTEMPTED INVALID SNAPSHOT:\n%s", invalid)
	err := e.Update([]byte(invalid))
	if err == nil {
		t.Fatal("invalid update must be rejected")
	}
	ce, ok := config.AsError(err)
	if !ok || ce.Kind != config.KindInvalidValue {
		t.Fatalf("want KindInvalidValue, got %v", err)
	}
	t.Logf("REJECTION BASIS: kind=%s path=%s msg=%q", ce.Kind, ce.Path, ce.Message)
	if e.ActiveVersion() != beforeVer || e.ActiveFingerprint() != beforeFP {
		t.Fatal("active version changed despite rejected update")
	}

	// A version label reused for different content is also rejected.
	sameLabel := `{"version":"v1","flags":{}}`
	err = e.Update([]byte(sameLabel))
	if err == nil {
		t.Fatal("reusing a label with different content must be rejected")
	}
	if ce, _ := config.AsError(err); ce.Kind != config.KindConflict {
		t.Fatalf("want KindConflict for label reuse, got %v", err)
	}
	if e.ActiveVersion() != beforeVer {
		t.Fatal("active version changed despite rejected label reuse")
	}
}

func TestRollbackReproducesEarlierDecision(t *testing.T) {
	e := mustEngine(t, evalCfg)
	cfgV1 := e.Snapshot()
	u := User{ID: "rollback-user-1"}

	rV1 := e.Evaluate(u, "rollout")
	logBasis(t, evalCfg, u, "rollout", rV1)

	// Move to v2 (rollout at a different percentage) then to v3.
	v2 := `{
	  "version": "v2",
	  "flags": {
	    "rollout": { "enabled": true, "percentage": 100 }
	  }
	}`
	v3 := `{
	  "version": "v3",
	  "flags": {
	    "rollout": { "enabled": false, "percentage": 100 }
	  }
	}`
	if err := e.Update([]byte(v2)); err != nil {
		t.Fatal(err)
	}
	rV2 := e.Evaluate(u, "rollout")
	t.Logf("V2 BASIS: on=%v reason=%s (v2 fp=%s)", rV2.On, rV2.Reason, e.ActiveFingerprint())
	if err := e.Update([]byte(v3)); err != nil {
		t.Fatal(err)
	}
	if r := e.Evaluate(u, "rollout"); r.On || r.Reason != ReasonFlagDisabled {
		t.Fatalf("v3 expected disabled, got %+v", r)
	}

	// Rollback to v1 by label: the decision must be byte-identical.
	if err := e.Rollback("v1"); err != nil {
		t.Fatal(err)
	}
	if e.ActiveFingerprint() != cfgV1.Fingerprint {
		t.Fatalf("rollback fingerprint = %s, want %s", e.ActiveFingerprint(), cfgV1.Fingerprint)
	}
	rAfter := e.Evaluate(u, "rollout")
	logBasis(t, evalCfg, u, "rollout(after rollback)", rAfter)
	if rAfter != rV1 {
		t.Fatalf("rollback changed decision: before=%+v after=%+v", rV1, rAfter)
	}
	t.Logf("rollback decision stable: on=%v reason=%s bucket=%d",
		rAfter.On, rAfter.Reason, rAfter.Bucket)
}

func TestRollbackUnknownVersion(t *testing.T) {
	e := mustEngine(t, evalCfg)
	if err := e.Rollback("nope"); err == nil {
		t.Fatal("rollback to unknown version must fail")
	}
}

func TestRollbackByFingerprint(t *testing.T) {
	e := mustEngine(t, evalCfg)
	fpV1 := e.ActiveFingerprint()
	if err := e.Update([]byte(`{"version":"vx","flags":{"f":{"enabled":true,"percentage":1}}}`)); err != nil {
		t.Fatal(err)
	}
	if e.ActiveVersion() != "vx" {
		t.Fatalf("active=%q want vx", e.ActiveVersion())
	}
	if err := e.RollbackToFingerprint(fpV1); err != nil {
		t.Fatal(err)
	}
	if e.ActiveFingerprint() != fpV1 || e.ActiveVersion() != "v1" {
		t.Fatalf("fingerprint rollback landed on %s/%s", e.ActiveVersion(), e.ActiveFingerprint())
	}
	if err := e.RollbackToFingerprint("deadbeef"); err == nil {
		t.Fatal("rollback to unknown fingerprint must fail")
	}
}
