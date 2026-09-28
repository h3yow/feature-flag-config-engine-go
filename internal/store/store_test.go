package store

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
)

const flagTpl = `{
  "version": "%s",
  "flags": [
    {"key": "f", "enabled": true, "default": false,
     "rollout": {"percentage": %d, "salt": "salt-%s"}}
  ]
}`

func config(version string, pct int) string {
	return fmt.Sprintf(flagTpl, version, pct, version)
}

func mustPublish(t *testing.T, st *Store, raw string) int64 {
	t.Helper()
	snap, err := st.PublishRaw([]byte(raw))
	if err != nil {
		t.Fatalf("PublishRaw 失败: %v\n快照:\n%s", err, raw)
	}
	t.Logf("已发布 revision=%d version=%q", snap.Revision, snap.Config.Version)
	return snap.Revision
}

func TestPublishRejectsInvalidAndKeepsOldVersion(t *testing.T) {
	st := New()
	rev1 := mustPublish(t, st, config("v1", 10))

	cases := []struct {
		name string
		raw  string
		kind errs.Kind
	}{
		{"语法错误", `{"version":"v2","flags":[}`, errs.KindSyntax},
		{"缺少 version", `{"flags":[]}`, errs.KindMissingField},
		{"取值非法", config("v2", 500), errs.KindInvalidValue},
		{"引用缺失", `{"version":"v2","flags":[
		  {"key":"a","enabled":true,"depends_on":["ghost"]}]}`, errs.KindReference},
		{"循环依赖", `{"version":"v2","flags":[
		  {"key":"a","enabled":true,"depends_on":["b"]},
		  {"key":"b","enabled":true,"depends_on":["a"]}]}`, errs.KindDependency},
		{"互斥不对称", `{"version":"v2","flags":[
		  {"key":"a","enabled":true,"default":true,"conflicts_with":["b"]},
		  {"key":"b","enabled":true,"default":true}]}`, errs.KindConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("输入配置快照:\n%s", tc.raw)
			_, err := st.PublishRaw([]byte(tc.raw))
			if err == nil {
				t.Fatalf("非法配置必须被拒绝")
			}
			if !errs.IsKind(err, tc.kind) {
				t.Fatalf("错误分类错误，期望 %s，实际: %v", tc.kind, err)
			}
			t.Logf("如预期被拒绝（kind=%s）: %v", tc.kind, err)
			// 旧版本必须原封不动。
			cur := st.Current()
			if cur.Revision != rev1 || cur.Config.Version != "v1" {
				t.Fatalf("拒绝非法配置后当前版本被污染: rev=%d version=%s",
					cur.Revision, cur.Config.Version)
			}
		})
	}
}

func TestDuplicateContentIsNoop(t *testing.T) {
	st := New()
	raw := config("v1", 10)
	r1 := mustPublish(t, st, raw)
	r2 := mustPublish(t, st, raw)
	if r1 != r2 {
		t.Fatalf("相同内容重复发布应返回同一 revision，%d != %d", r1, r2)
	}
}

func TestConcurrentPublishAndRead(t *testing.T) {
	st := New()
	mustPublish(t, st, config("v0", 0))

	var stop atomic.Bool
	var wg sync.WaitGroup

	// 并发读者：永远只能看到结构完整的快照，且 bucket/percent 关系自洽。
	reads := int64(0)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				snap := st.Current()
				if snap == nil || snap.Config == nil || len(snap.Config.Flags) != 1 {
					t.Errorf("读到不完整快照: %+v", snap)
					return
				}
				f := snap.Config.Flags[0]
				if f.Rollout == nil || f.Rollout.Percentage < 0 || f.Rollout.Percentage > 100 {
					t.Errorf("读到字段不完整的中间状态: %+v", f)
					return
				}
				atomic.AddInt64(&reads, 1)
			}
		}()
	}

	// 发布者：交替发布合法版本与非法版本。
	validVersions := []int{0, 25, 50, 75, 100}
	for round := 0; round < 200; round++ {
		v := validVersions[round%len(validVersions)]
		raw := fmt.Sprintf(`{
		  "version": "r%d",
		  "flags": [{"key":"f","enabled":true,"default":false,
		    "rollout":{"percentage":%d,"salt":"s"}}]
		}`, round, v)
		if _, err := st.PublishRaw([]byte(raw)); err != nil {
			t.Errorf("合法版本发布失败: %v", err)
		}
		// 非法版本必须被拒绝且无副作用。
		if _, err := st.PublishRaw([]byte(`{"version":"bad","flags":[{"key":"f","enabled":true,
		  "rollout":{"percentage":999,"salt":"s"}}]}`)); err == nil {
			t.Errorf("非法版本竟然发布成功")
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("并发期间完成无锁读取 %d 次，未观察到中间状态", reads)
	if reads == 0 {
		t.Fatal("没有发生任何读取")
	}
}

func TestRollbackRestoresVersion(t *testing.T) {
	st := New()
	r1 := mustPublish(t, st, config("v1", 10))
	r2 := mustPublish(t, st, config("v2", 90))
	r3 := mustPublish(t, st, config("v3", 30))
	_ = r2

	if st.Current().Revision != r3 {
		t.Fatal("当前应为最新版本")
	}
	snap, err := st.Rollback(r1)
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if snap.Revision != r1 || snap.Config.Version != "v1" {
		t.Fatalf("回滚后版本不正确: %+v", snap)
	}

	// 回滚不产生新版本，历史保持不变。
	vs := st.Versions()
	if len(vs) != 3 || vs[0].Revision != r1 {
		t.Fatalf("回滚不应改变历史，实际: %+v", vs)
	}

	// 回滚到不存在的修订号应失败。
	if _, err := st.Rollback(9999); !errs.IsKind(err, errs.KindState) {
		t.Fatalf("回滚不存在版本应返回 state 错误，实际: %v", err)
	}
}

func TestRollbackEmptyStore(t *testing.T) {
	st := New()
	if _, err := st.Rollback(1); !errs.IsKind(err, errs.KindState) {
		t.Fatalf("空 store 回滚应返回 state 错误，实际: %v", err)
	}
}
