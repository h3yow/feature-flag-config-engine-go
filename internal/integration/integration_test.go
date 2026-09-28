package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/h3yow/feature-flag-config-engine-go/internal/engine"
	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
	"github.com/h3yow/feature-flag-config-engine-go/internal/source"
	"github.com/h3yow/feature-flag-config-engine-go/internal/store"
)

func writeConfig(t *testing.T, path, version string, pct int) {
	t.Helper()
	raw := fmt.Sprintf(`{
	  "version": %q,
	  "flags": [
	    {"key": "rollout-flag", "enabled": true, "default": false,
	     "rollout": {"percentage": %d, "salt": "shared-salt"}},
	    {"key": "always-on", "enabled": true, "default": true},
	    {"key": "child", "enabled": true, "default": false,
	     "depends_on": ["rollout-flag"],
	     "targeting": [
	       {"name": "internal",
	        "match": {"attr": "tier", "op": "eq", "values": ["internal"]},
	        "serve": true}
	     ],
	     "rollout": {"percentage": 0, "salt": "child-salt"}}
	  ]
	}`, version, pct)
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	t.Logf("写入配置快照(%s, pct=%d):\n%s", version, pct, raw)
}

func snapshotDecisions(t *testing.T, eng *engine.Engine, users []engine.User) map[string]engine.Decision {
	out := map[string]engine.Decision{}
	for _, u := range users {
		d, err := eng.Eval("rollout-flag", u)
		if err != nil {
			t.Fatalf("判定失败 user=%s: %v", u.ID, err)
		}
		out[u.ID] = d
		t.Logf("判定依据 user=%s => on=%v bucket=%d reason=%s", u.ID, d.On, d.Bucket, d.Reason)
	}
	return out
}

func TestHotReloadAndRollbackConsistency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, "v1", 10)

	st := store.New()
	eng := engine.New(st)

	// 初始加载。
	src := source.NewFileSource(path)
	raw0, err := src.Read()
	if err != nil {
		t.Fatal(err)
	}
	snap1, err := st.PublishRaw(raw0)
	if err != nil {
		t.Fatalf("初始加载失败: %v", err)
	}

	users := makeUsers(200)
	decV1 := snapshotDecisions(t, eng, users)

	// 先启动文件监听（短轮询，加快测试），确保初始缓存就是 v1，再写 v2。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reloaded := make(chan int64, 8)
	w := source.NewWatcher(src, func(raw []byte) error {
		snap, err := st.PublishRaw(raw)
		if err != nil {
			return err
		}
		select {
		case reloaded <- snap.Revision:
		default:
		}
		return nil
	}, source.WithPollInterval(20*time.Millisecond))
	watchDone := make(chan struct{})
	go func() { defer close(watchDone); _ = w.Start(ctx) }()
	time.Sleep(60 * time.Millisecond)

	// 热更新到 v2（50%），等待监听生效。
	writeConfig(t, path, "v2", 50)
	select {
	case rev := <-reloaded:
		t.Logf("热更新生效 revision=%d", rev)
	case <-time.After(2 * time.Second):
		t.Fatal("未观察到 v2 热更新")
	}
	if st.Current().Config.Version != "v2" {
		t.Fatalf("热更新后版本应为 v2，实际 %s", st.Current().Config.Version)
	}
	decV2 := snapshotDecisions(t, eng, users)
	changed := 0
	for _, u := range users {
		if decV1[u.ID].On != decV2[u.ID].On {
			changed++
		}
	}
	t.Logf("v1(10%%) -> v2(50%%) 结果发生变化的用户数=%d", changed)
	if changed == 0 {
		t.Fatal("放量扩大后应至少有用户从 off 变 on")
	}

	// 写入一份非法配置（语法错误）：必须被整体拒绝，当前仍是 v2。
	bad := []byte(`{ "version": "broken", "flags": [ }`)
	t.Logf("写入非法配置快照:\n%s", string(bad))
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if st.Current().Config.Version != "v2" {
		t.Fatalf("非法配置被拒绝后仍应停留在 v2，实际 %s", st.Current().Config.Version)
	}
	decAfterBad := snapshotDecisions(t, eng, users)
	for _, u := range users {
		if decAfterBad[u.ID].On != decV2[u.ID].On {
			t.Fatal("拒绝非法更新期间判定结果不应改变")
		}
	}
	t.Log("非法更新已被整体拒绝，判定结果与 v2 完全一致")

	// 再写一份取值非法（percentage 越界 + 字段缺失）的配置，同样必须拒绝。
	invalid2 := `{"version":"bad2","flags":[
	  {"enabled":true,"rollout":{"percentage":101,"salt":"x"}},
	  {"key":"k","enabled":true,"targeting":[{"name":"r","match":{"attr":"a","op":"eq","values":["1"]}}]}
	]}`
	t.Logf("写入非法配置快照:\n%s", invalid2)
	if err := os.WriteFile(path, []byte(invalid2), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if st.Current().Config.Version != "v2" {
		t.Fatalf("第二类非法配置也必须被拒绝，实际版本=%s", st.Current().Config.Version)
	}

	// 修复文件 -> v3（30%），无需重启即可再次生效。
	writeConfig(t, path, "v3", 30)
	select {
	case <-reloaded:
	case <-time.After(2 * time.Second):
		t.Fatal("修复后未观察到热更新")
	}
	_ = snap1
	if st.Current().Config.Version != "v3" {
		t.Fatalf("修复后应自动切到 v3，实际 %s", st.Current().Config.Version)
	}

	// 回滚到 v1：同一批评定结果必须与当时完全一致。
	v1rev := st.Versions()[0].Revision
	rb, err := st.Rollback(v1rev)
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	t.Logf("已回滚到 revision=%d version=%s", rb.Revision, rb.Config.Version)
	decRollback := snapshotDecisions(t, eng, users)
	for _, u := range users {
		a, b := decV1[u.ID], decRollback[u.ID]
		if a.On != b.On || a.Bucket != b.Bucket || a.Reason != b.Reason {
			t.Fatalf("回滚后用户 %s 判定与 v1 当时不一致: %+v vs %+v", u.ID, a, b)
		}
	}
	t.Log("回滚后 200 个用户的 on/bucket/reason 与 v1 当时逐一一致")
}

func TestErrorKindsAreDistinguishable(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		kind errs.Kind
	}{
		{"语法", `{not-json`, errs.KindSyntax},
		{"字段缺失", `{"version":"v","flags":[{"key":"f","enabled":true,
		  "targeting":[{"name":"r","match":{"attr":"a","op":"eq","values":["1"]}}]}]}`,
			errs.KindMissingField},
		{"取值非法", `{"version":"v","flags":[{"key":"f","enabled":true,
		  "rollout":{"percentage":-1,"salt":"s"}}]}`, errs.KindInvalidValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("输入配置快照:\n%s", tc.raw)
			st := store.New()
			_, err := st.PublishRaw([]byte(tc.raw))
			if err == nil {
				t.Fatal("必须报错")
			}
			if !errs.IsKind(err, tc.kind) {
				t.Fatalf("期望分类 %s，实际: %v", tc.kind, err)
			}
			t.Logf("错误可区分为 %s: %v", tc.kind, err)
		})
	}
}

func TestConcurrentEvalWhileReloading(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfig(t, path, "v1", 50)

	st := store.New()
	eng := engine.New(st)
	src := source.NewFileSource(path)
	raw, _ := src.Read()
	if _, err := st.PublishRaw(raw); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := source.NewWatcher(src, func(b []byte) error {
		_, err := st.PublishRaw(b)
		return err
	}, source.WithPollInterval(10*time.Millisecond))
	go func() { _ = w.Start(ctx) }()

	var stop atomic.Bool
	var wg sync.WaitGroup
	var evals atomic.Int64

	// 判定者：高频并发判定，只接受合法结果（桶号在 [0,99]）或明确错误。
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for n := 0; !stop.Load(); n++ {
				uid := fmt.Sprintf("user-%d", n%500)
				d, err := eng.Eval("rollout-flag", engine.User{ID: uid})
				if err != nil {
					t.Errorf("意外判定错误: %v", err)
					return
				}
				if d.Bucket < 0 || d.Bucket > 99 {
					t.Errorf("读到非法桶号: %d", d.Bucket)
					return
				}
				evals.Add(1)
			}
		}(i)
	}

	// 更新者：持续发布 0/25/50/75/100 与非法版本。
	go func() {
		pcts := []int{0, 25, 50, 75, 100}
		for i := 0; i < 100 && !stop.Load(); i++ {
			writeConfig(t, path, fmt.Sprintf("hot-%d", i), pcts[i%5])
			_ = os.WriteFile(path, []byte(`{"version":"x","flags":[}`), 0o644)
			time.Sleep(2 * time.Millisecond)
		}
	}()

	time.Sleep(800 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	t.Logf("并发热更新期间完成判定 %d 次，无中间态、无阻塞异常", evals.Load())
	if evals.Load() == 0 {
		t.Fatal("没有完成任何判定")
	}
}

func makeUsers(n int) []engine.User {
	out := make([]engine.User, n)
	for i := range out {
		out[i] = engine.User{ID: fmt.Sprintf("user-%04d", i), Attrs: map[string]string{}}
	}
	return out
}
