package bucket

import "testing"

func TestBucketStableAndBounded(t *testing.T) {
	const fp = "fp-v1"
	b1 := Bucket("new_checkout", fp, "user-1")
	b2 := Bucket("new_checkout", fp, "user-1")
	if b1 != b2 {
		t.Fatalf("bucket not stable: %d vs %d", b1, b2)
	}
	if b1 < 0 || b1 >= 100 {
		t.Fatalf("bucket out of range: %d", b1)
	}
}

func TestBucketSpreadsAcrossHundred(t *testing.T) {
	// Every bucket must be reachable over enough synthetic users and
	// no bucket may be wildly over/under-represented.
	counts := make([]int, 100)
	const n = 100000
	for i := 0; i < n; i++ {
		counts[Bucket("flag", "fp", userID(i))]++
	}
	empty, max := 0, 0
	for _, c := range counts {
		if c == 0 {
			empty++
		}
		if c > max {
			max = c
		}
	}
	if empty != 0 {
		t.Fatalf("some buckets never reached: %d empty buckets", empty)
	}
	// Expected ~1000 per bucket; allow a generous 30% spread.
	if float64(max) > 1.3*float64(n)/100 {
		t.Fatalf("bucketing too skewed, max count %d", max)
	}
	t.Logf("bucket spread OK, max bucket occupancy=%d (ideal=%d)", max, n/100)
}

func TestBucketVersionAndFlagDependence(t *testing.T) {
	// Different flags must not perfectly correlate for the same user.
	var differFlags, differVersion int
	const n = 2000
	for i := 0; i < n; i++ {
		u := userID(i)
		if Bucket("flag-a", "fp", u) != Bucket("flag-b", "fp", u) {
			differFlags++
		}
		if Bucket("flag-a", "fp-v1", u) != Bucket("flag-a", "fp-v2", u) {
			differVersion++
		}
	}
	// Independent mixing should make ~99% differ. Require >95% to
	// catch accidental total correlation without being flaky.
	if differFlags < n*95/100 {
		t.Fatalf("flags correlate too strongly: %d/%d differ", differFlags, n)
	}
	if differVersion < n*95/100 {
		t.Fatalf("versions correlate too strongly: %d/%d differ", differVersion, n)
	}
}

func userID(i int) string {
	return "user-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestMatchCondition(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		exists bool
		op     string
		values []string
		want   bool
	}{
		{"eq hit", "staff", true, "eq", []string{"staff"}, true},
		{"eq miss", "free", true, "eq", []string{"staff"}, false},
		{"eq missing attr", "", false, "eq", []string{"staff"}, false},
		{"neq hit", "free", true, "neq", []string{"staff"}, true},
		{"neq missing attr", "", false, "neq", []string{"staff"}, true},
		{"in hit", "gold", true, "in", []string{"gold", "plat"}, true},
		{"in miss", "free", true, "in", []string{"gold", "plat"}, false},
		{"in missing", "", false, "in", []string{"gold"}, false},
		{"not_in hit", "free", true, "not_in", []string{"gold"}, true},
		{"not_in missing", "", false, "not_in", []string{"gold"}, true},
		{"unknown op", "x", true, "regex", []string{"x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("MATCH INPUT: value=%q exists=%v op=%s values=%v", tc.value, tc.exists, tc.op, tc.values)
			got := MatchCondition(tc.value, tc.exists, tc.op, tc.values)
			t.Logf("MATCH BASIS/RESULT: %v (want %v)", got, tc.want)
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
