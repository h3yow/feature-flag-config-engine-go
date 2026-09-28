package store

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"

	"github.com/h3yow/feature-flag-config-engine-go/internal/compile"
	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
	"github.com/h3yow/feature-flag-config-engine-go/internal/spec"
)

// maxHistory 是保留的最近版本数（含当前版本）。
const maxHistory = 50

// VersionInfo 描述历史中的一个版本。
type VersionInfo struct {
	Revision   int64
	ContentSHA string
	Version    string
}

// Store 持有当前快照与版本历史。
// 读取（Current）走 atomic，完全无锁；发布/回滚在写锁内完成且只做一次原子替换，
// 因此并发判定永远不会读到更新到一半的中间状态。
type Store struct {
	current atomic.Pointer[compile.Snapshot]

	mu      sync.Mutex
	nextRev int64
	history []*compile.Snapshot // 最近版本，旧 -> 新
}

func New() *Store {
	return &Store{}
}

// ContentSHA 返回原始配置内容的 SHA-256（十六进制），用于版本去重与排查。
func ContentSHA(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// PublishRaw 解析 + 校验 + 编译 + 原子发布。任何一步失败都不会影响当前版本。
// 若内容与当前版本完全相同（SHA 一致），视为无变化，直接返回当前快照。
func (s *Store) PublishRaw(raw []byte) (*compile.Snapshot, error) {
	cfg, err := spec.ParseJSON(raw)
	if err != nil {
		return nil, err
	}
	snap, err := compile.Compile(cfg)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sha := ContentSHA(raw)
	if cur := s.current.Load(); cur != nil && cur.ContentSHA == sha {
		return cur, nil
	}

	s.nextRev++
	snap = snap.WithRevision(s.nextRev, sha)
	s.history = append(s.history, snap)
	if len(s.history) > maxHistory {
		s.history = s.history[len(s.history)-maxHistory:]
	}
	s.current.Store(snap)
	return snap, nil
}

// Current 无锁返回当前快照；尚未发布任何配置时返回 nil。
func (s *Store) Current() *compile.Snapshot { return s.current.Load() }

// Rollback 切换到历史中的指定修订号。回滚不制造新版本、不改变历史，
// 只是把当前指针整体替换为旧快照，因此回滚后判定结果与该版本当时完全一致。
func (s *Store) Rollback(revision int64) (*compile.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.history) == 0 {
		return nil, errs.New(errs.KindState, "no_version", "", "尚无已发布版本，无法回滚")
	}
	for _, snap := range s.history {
		if snap.Revision == revision {
			s.current.Store(snap)
			return snap, nil
		}
	}
	return nil, errs.New(errs.KindState, "revision_not_found", "",
		"修订号不在可回滚范围内（仅保留最近版本）")
}

// Versions 返回历史版本信息（旧 -> 新），当前版本以最后一个元素体现。
func (s *Store) Versions() []VersionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]VersionInfo, 0, len(s.history))
	for _, snap := range s.history {
		out = append(out, VersionInfo{
			Revision:   snap.Revision,
			ContentSHA: snap.ContentSHA,
			Version:    snap.Config.Version,
		})
	}
	return out
}
