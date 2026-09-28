package source

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
)

func TestWatcherFiresOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	if err := AtomicWrite(path, []byte(`{"version":"v1"}`)); err != nil {
		t.Fatal(err)
	}
	src := NewFileSource(path)

	var called atomic.Int32
	w := NewWatcher(src, func(raw []byte) error {
		called.Add(1)
		t.Logf("onChange 触发，内容: %s", string(raw))
		return nil
	}, WithPollInterval(15*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	time.Sleep(60 * time.Millisecond)
	// 重写相同内容不应触发。
	if err := AtomicWrite(path, []byte(`{"version":"v1"}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if got := called.Load(); got != 0 {
		t.Fatalf("内容未变不应触发回调，实际 %d 次", got)
	}

	if err := AtomicWrite(path, []byte(`{"version":"v2"}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for called.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if called.Load() != 1 {
		t.Fatalf("内容变化应触发恰好 1 次回调，实际 %d", called.Load())
	}
}

func TestWatcherSurvivesRejectedChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	good1 := []byte(`{"version":"v1","flags":[]}`)
	if err := os.WriteFile(path, good1, 0o644); err != nil {
		t.Fatal(err)
	}
	src := NewFileSource(path)

	// 模拟 store：JSON 解析失败即整体拒绝（返回分类错误），否则接受。
	var accepted atomic.Int32
	onChange := func(raw []byte) error {
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			return errs.Wrap(err, errs.KindSyntax, "json_syntax", "", "模拟解析失败")
		}
		accepted.Add(1)
		t.Logf("接受版本: %s", string(raw))
		return nil
	}
	var sawErr atomic.Int32
	w := NewWatcher(src, onChange,
		WithPollInterval(15*time.Millisecond),
		WithErrorHandler(func(err error) {
			sawErr.Add(1)
			t.Logf("错误处理器收到（预期）: %v", err)
		}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()
	time.Sleep(60 * time.Millisecond)

	// 先确认 watcher 已观测到一次合法变更，再写坏内容，避免轮询漏掉中间版本。
	if err := AtomicWrite(path, []byte(`{"version":"v2","flags":[]}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for accepted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if accepted.Load() != 1 {
		t.Fatalf("准备阶段 v2 应被接受，accepted=%d", accepted.Load())
	}

	// 写入坏内容 -> 被回调拒绝，但监听继续。
	if err := AtomicWrite(path, []byte(`{broken`)); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for sawErr.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if sawErr.Load() == 0 {
		t.Fatal("坏内容应触发错误处理器")
	}
	if accepted.Load() != 1 {
		t.Fatalf("坏内容不应产生新的接受，accepted=%d（预期仍为 1）", accepted.Load())
	}

	// 修复文件 -> 无需重启即被接受。
	if err := AtomicWrite(path, []byte(`{"version":"v3","flags":[]}`)); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for accepted.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if accepted.Load() != 2 {
		t.Fatalf("修复后应被接受，accepted=%d", accepted.Load())
	}
}

func TestReadMissingFile(t *testing.T) {
	src := NewFileSource(filepath.Join(t.TempDir(), "nope.json"))
	if _, err := src.Read(); !errs.IsKind(err, errs.KindState) {
		t.Fatalf("读取缺失文件应得到 state 错误，实际: %v", err)
	}
}
