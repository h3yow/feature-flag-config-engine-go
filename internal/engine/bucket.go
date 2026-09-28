package engine

import "hash/fnv"

// bucket 对 (salt, flagKey, userID) 做稳定哈希，返回 [0,99] 的桶号。
//
// 同一配置版本下同一用户同一开关的桶号恒定；salt 保证不同开关的分桶相互独立。
// 放量区间为 [0, percentage)：percentage=0 无人命中，percentage=100 全部命中。
func bucket(salt, flagKey, userID string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(flagKey))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(userID))
	return int(h.Sum64() % 100)
}
