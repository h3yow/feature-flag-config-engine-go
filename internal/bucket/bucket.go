package bucket

import (
	"hash/fnv"
)

// User carries the identity and attributes used for matching and bucketing.
type User struct {
	ID         string
	Attributes map[string]string
}

// Bucket returns the stable bucket of a user for one flag, in [0,100).
//
// It is a PURE function of (flagKey, fingerprint, userID):
//
//   - flagKey is mixed in so two independent flags spread the same user
//     independently instead of always sharing a bucket;
//   - fingerprint is the config-version content hash, so a rollback to
//     the same content yields exactly the same buckets as before, while
//     a genuinely new version is free to re-shuffle rollouts;
//   - userID is the stable user identity.
//
// A 64-bit FNV-1a hash is reduced with 1000000-way fan-out then
// modulo 100, keeping per-bucket deviation under 1e-6 (no modulo bias
// that could matter at percentage boundaries).
func Bucket(flagKey, fingerprint, userID string) int {
	h := fnv.New64a()
	h.Write([]byte(flagKey))
	h.Write([]byte{0x1f})
	h.Write([]byte(fingerprint))
	h.Write([]byte{0x1f})
	h.Write([]byte(userID))
	sum := h.Sum64()
	return int((sum % 1000000) / 10000) // 0..99
}

// MatchCondition reports whether a user attribute satisfies a condition.
// Missing attributes never satisfy positive operators (eq, in) but do
// satisfy negative ones (neq, not_in), i.e. "not equal to X" holds when
// the attribute is absent.
func MatchCondition(attrValue string, exists bool, op string, values []string) bool {
	switch op {
	case "eq":
		return exists && len(values) > 0 && attrValue == values[0]
	case "neq":
		return !exists || len(values) == 0 || attrValue != values[0]
	case "in":
		if !exists {
			return false
		}
		for _, v := range values {
			if attrValue == v {
				return true
			}
		}
		return false
	case "not_in":
		if !exists {
			return true
		}
		for _, v := range values {
			if attrValue == v {
				return false
			}
		}
		return true
	default:
		return false
	}
}
