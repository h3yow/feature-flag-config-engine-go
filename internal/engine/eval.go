package engine

import (
	"github.com/h3yow/feature-flag-config-engine-go/internal/bucket"
	"github.com/h3yow/feature-flag-config-engine-go/internal/config"
)

// Reason explains why a flag resolved the way it did; it makes every
// decision auditable and reproducible.
type Reason string

const (
	ReasonFlagDisabled   Reason = "flag_disabled"
	ReasonExcluded       Reason = "excluded_audience"
	ReasonIncluded       Reason = "included_audience"
	ReasonDependencyOff  Reason = "dependency_off"
	ReasonConflictActive Reason = "conflict_active"
	ReasonInRollout      Reason = "in_percentage_rollout"
	ReasonOutOfRollout   Reason = "out_of_percentage_rollout"
	ReasonUnknownFlag    Reason = "unknown_flag"
)

// Result is one flag decision plus its evidence (bucket, matched
// audience, related flag) so tests and logs can print the full basis.
type Result struct {
	On          bool
	Reason      Reason
	Bucket      int
	MatchedAud  string
	RelatedFlag string
	// Version and Fingerprint identify the immutable snapshot this
	// decision was computed on, captured atomically with the decision.
	Version     string
	Fingerprint string
}

// Evaluate decides one flag for one user against the active snapshot.
// It takes a single atomic snapshot pointer at entry, so an update
// racing with this call cannot mix rules from two config versions.
func (e *Engine) Evaluate(user User, flagKey string) Result {
	s := e.active.Load()
	if s == nil {
		return Result{Reason: ReasonUnknownFlag, RelatedFlag: flagKey}
	}
	r := eval(s.cfg, user, flagKey, nil)
	r.Version = s.cfg.Version
	r.Fingerprint = s.cfg.Fingerprint
	return r
}

// evalFrame tracks the dependency path of a single evaluation to guard
// against pathological recursion (config validation already rejects
// cycles and unknown references, this is defence in depth).
type evalFrame struct {
	stack map[string]bool
}

func eval(cfg *config.Config, user User, flagKey string, frame *evalFrame) Result {
	f, ok := cfg.Flags[flagKey]
	if !ok {
		// Configs are validated on load, so this only happens if a
		// dependency/conflict reference is somehow inconsistent:
		// fail closed rather than silently serving ON.
		return Result{Reason: ReasonUnknownFlag, RelatedFlag: flagKey}
	}
	if frame == nil {
		frame = &evalFrame{stack: map[string]bool{}}
	}
	if frame.stack[flagKey] {
		// Fail closed on any unexpected evaluation cycle.
		return Result{Reason: ReasonDependencyOff, RelatedFlag: flagKey}
	}
	frame.stack[flagKey] = true
	defer delete(frame.stack, flagKey)

	// 1. Master switch.
	if !f.Enabled {
		return Result{Reason: ReasonFlagDisabled}
	}

	// 2. Explicit audience exclusions win over everything else.
	if aud, ok := matchAnyAudience(cfg, user, f.ExcludeAudiences); ok {
		return Result{Reason: ReasonExcluded, MatchedAud: aud}
	}

	// 3. Dependencies: every depends_on flag must itself be ON.
	for _, dep := range f.DependsOn {
		r := eval(cfg, user, dep, frame)
		if !r.On {
			return Result{Reason: ReasonDependencyOff, RelatedFlag: dep}
		}
	}

	// 4. Mutual exclusion. When two conflicting flags are both
	// eligible for the same user, a deterministic rule picks a single
	// winner so the decision stays stable and reproducible: the flag
	// whose key sorts lexicographically smallest wins. Consequently f
	// is suppressed only by a conflicting flag with a smaller key that
	// itself evaluates ON; that recursion never returns to f (conflict
	// edges only flow toward smaller keys).
	for _, other := range sortedStrings(f.ConflictsWith) {
		if other >= flagKey {
			continue
		}
		r := eval(cfg, user, other, frame)
		if r.On {
			return Result{Reason: ReasonConflictActive, RelatedFlag: other}
		}
	}

	// 5. Explicit audience inclusions.
	if aud, ok := matchAnyAudience(cfg, user, f.IncludeAudiences); ok {
		return Result{On: true, Reason: ReasonIncluded, MatchedAud: aud}
	}

	// 6. Stable percentage rollout. bucket < percentage => ON.
	b := bucket.Bucket(flagKey, cfg.Fingerprint, user.ID)
	if b < f.Percentage {
		return Result{On: true, Reason: ReasonInRollout, Bucket: b}
	}
	return Result{On: false, Reason: ReasonOutOfRollout, Bucket: b}
}

// matchAnyAudience returns the first (config-order-independent but
// deterministic) referenced audience whose all conditions match.
func matchAnyAudience(cfg *config.Config, user User, names []string) (string, bool) {
	for _, name := range names {
		a, ok := cfg.Audiences[name]
		if !ok {
			continue
		}
		if audienceMatches(a, user) {
			return name, true
		}
	}
	return "", false
}

func audienceMatches(a *config.Audience, user User) bool {
	for _, c := range a.Conditions {
		v, exists := lookupAttr(user, c.Attribute)
		if !bucket.MatchCondition(v, exists, c.Operator, c.Values) {
			return false // conditions within one audience are AND-ed
		}
	}
	return true
}

func lookupAttr(user User, attr string) (string, bool) {
	if user.Attributes == nil {
		return "", false
	}
	v, ok := user.Attributes[attr]
	return v, ok
}

func sortedStrings(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	out := append([]string(nil), in...)
	// insertion sort is fine for the tiny slices seen here.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
