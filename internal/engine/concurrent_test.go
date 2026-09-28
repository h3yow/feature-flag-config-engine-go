package engine

import (
	"sync"
	"sync/atomic"
	"testing"
)

const (
	concV1  = `{"version":"c-v1","flags":{"f":{"enabled":true,"percentage":100}}}`
	concV2  = `{"version":"c-v2","flags":{"f":{"enabled":false,"percentage":100}}}`
	concBad = `{"version":"c-bad","flags":{"f":{"enabled":true,"percentage":420}}}`
)

func TestConcurrentUpdatesAndEvaluations(t *testing.T) {
	e := mustEngine(t, concV1)

	var stop atomic.Bool
	var wg sync.WaitGroup
	rejections := int64(0)

	// Readers: every decision must be explainable by exactly one fully
	// applied version: either v1 (on=true, in_rollout) or v2
	// (disabled). There is no third legal state, so a torn/intermediate
	// state would show up as an impossible reason combination.
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := User{ID: "concurrency-user"}
			for !stop.Load() {
				r := e.Evaluate(u, "f")
				// Version and result come from one atomic snapshot;
				// cross-check them against each other only.
				switch r.Version {
				case "c-v1":
					if !r.On || r.Reason != ReasonInRollout {
						t.Errorf("inconsistent decision under c-v1: %+v", r)
						return
					}
				case "c-v2":
					if r.On || r.Reason != ReasonFlagDisabled {
						t.Errorf("inconsistent decision under c-v2: %+v", r)
						return
					}
				default:
					t.Errorf("decision on unknown version %q", r.Version)
					return
				}
				if r.Fingerprint == "" {
					t.Error("missing fingerprint on decision")
					return
				}
			}
		}()
	}

	// Writers: alternate valid versions and pepper in invalid ones.
	wg.Add(1)
	go func() {
		defer wg.Done()
		docs := [][]byte{[]byte(concV2), []byte(concV1)}
		for i := 0; i < 2000; i++ {
			if i%7 == 0 {
				if err := e.Update([]byte(concBad)); err == nil {
					t.Error("invalid update was accepted")
					return
				}
				atomic.AddInt64(&rejections, 1)
				continue
			}
			if err := e.Update(docs[i%2]); err != nil {
				t.Errorf("valid update rejected: %v", err)
				return
			}
		}
		stop.Store(true)
	}()

	wg.Wait()
	if rejections == 0 {
		t.Fatal("expected some rejected updates")
	}
	t.Logf("concurrent run complete; invalid updates rejected=%d; final version=%s",
		rejections, e.ActiveVersion())

	// After rollback to c-v1 decisions must match the earlier state.
	if err := e.Rollback("c-v1"); err != nil {
		t.Fatal(err)
	}
	r := e.Evaluate(User{ID: "concurrency-user"}, "f")
	t.Logf("post-rollback BASIS: on=%v reason=%s", r.On, r.Reason)
	if !r.On || r.Reason != ReasonInRollout {
		t.Fatalf("rollback after concurrent updates broke: %+v", r)
	}
}
