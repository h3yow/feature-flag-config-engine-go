package compile

// WithRevision 返回带修订号与内容哈希的快照副本。发布前快照由 store 统一编号。
func (s *Snapshot) WithRevision(rev int64, sha string) *Snapshot {
	cp := *s
	cp.Revision = rev
	cp.ContentSHA = sha
	return &cp
}
