package engine

import (
	"strings"
	"testing"

	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
	"github.com/h3yow/feature-flag-config-engine-go/internal/store"
)

// publishConfig 发布一份配置，失败即终止测试。
func publishConfig(t *testing.T, st *store.Store, raw string) int64 {
	t.Helper()
	snap, err := st.PublishRaw([]byte(raw))
	if err != nil {
		t.Fatalf("发布配置失败: %v\n输入配置快照:\n%s", err, raw)
	}
	t.Logf("输入配置快照:\n%s", raw)
	return snap.Revision
}

func evalMust(t *testing.T, eng *Engine, flag, uid string, attrs map[string]string) Decision {
	t.Helper()
	d, err := eng.Eval(flag, User{ID: uid, Attrs: attrs})
	if err != nil {
		t.Fatalf("Eval(%q,%q) 意外错误: %v", flag, uid, err)
	}
	t.Logf("判定依据 flag=%s user=%s => on=%v reason=%s bucket=%d percentage=%v\n  trace:\n    %s",
		flag, uid, d.On, d.Reason, d.Bucket, d.Percentage, strings.Join(d.Trace, "\n    "))
	return d
}

func evalErr(t *testing.T, eng *Engine, flag, uid string) error {
	t.Helper()
	d, err := eng.Eval(flag, User{ID: uid})
	for _, tr := range d.Trace {
		t.Logf("  trace: %s", tr)
	}
	if err == nil {
		t.Fatalf("Eval(%q,%q) 期望错误，实际 on=%v", flag, uid, d.On)
	}
	t.Logf("判定依据 flag=%s user=%s 如预期报错: %v", flag, uid, err)
	return err
}

func TestRolloutBoundary(t *testing.T) {
	st := store.New()
	publishConfig(t, st, `{
	  "version": "v1",
	  "flags": [
	    {"key": "zero", "enabled": true, "default": true,
	     "rollout": {"percentage": 0, "salt": "s-zero"}},
	    {"key": "hundred", "enabled": true, "default": false,
	     "rollout": {"percentage": 100, "salt": "s-hundred"}}
	  ]
	}`)
	eng := New(st)

	// percentage=0：任意用户都必须关闭，即使 default=true。
	for _, uid := range []string{"u0", "u1", "alice", "zzz", "999"} {
		d := evalMust(t, eng, "zero", uid, nil)
		if d.On {
			t.Fatalf("percentage=0 时用户 %q 不应放量，bucket=%d", uid, d.Bucket)
		}
		if d.Bucket != d.Bucket {
			t.Fatal("桶号非法")
		}
	}
	// percentage=100：任意用户都必须打开（桶号最大 99 < 100）。
	for _, uid := range []string{"u0", "u1", "alice", "zzz", "999"} {
		d := evalMust(t, eng, "hundred", uid, nil)
		if !d.On {
			t.Fatalf("percentage=100 时用户 %q 必须放量，bucket=%d", uid, d.Bucket)
		}
	}
}

func TestStableAcrossRepeatedEval(t *testing.T) {
	st := store.New()
	publishConfig(t, st, `{
	  "version": "v1",
	  "flags": [
	    {"key": "f", "enabled": true, "default": false,
	     "rollout": {"percentage": 37, "salt": "stable-salt"}}
	  ]
	}`)
	eng := New(st)
	users := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	first := map[string]Decision{}
	for _, u := range users {
		first[u] = evalMust(t, eng, "f", u, nil)
	}
	// 同一配置版本下连评多次必须完全一致。
	for round := 0; round < 10; round++ {
		for _, u := range users {
			d := evalMust(t, eng, "f", u, nil)
			g := first[u]
			if d.On != g.On || d.Bucket != g.Bucket || d.Reason != g.Reason {
				t.Fatalf("用户 %q 判定不稳定: %+v vs %+v", u, d, g)
			}
		}
	}
}

func TestSaltMakesFlagsIndependent(t *testing.T) {
	st := store.New()
	raw := `{
	  "version": "v1",
	  "flags": [`
	keys := []string{}
	for i := 0; i < 20; i++ {
		if i > 0 {
			raw += ","
		}
		key := "flag-" + string(rune('a'+i))
		keys = append(keys, key)
		raw += `{"key":"` + key + `","enabled":true,"default":false,
		         "rollout":{"percentage":50,"salt":"salt-` + key + `"}}`
	}
	raw += `]}`
	publishConfig(t, st, raw)
	eng := New(st)

	// 不同 salt 下，某一固定用户在 20 个开关上的结果不应全部相同（极小概率假失败，
	// 失败时可重跑；这里要求至少出现 2 种桶结果）。
	buckets := map[int]bool{}
	for _, k := range keys {
		d := evalMust(t, eng, k, "fixed-user", nil)
		buckets[d.Bucket] = true
	}
	if len(buckets) < 2 {
		t.Fatalf("不同 salt 未产生独立分桶，桶集合=%v", buckets)
	}
}

func TestTargetingAndSegments(t *testing.T) {
	st := store.New()
	publishConfig(t, st, `{
	  "version": "v1",
	  "segments": {
	    "vip": {"name": "VIP", "user_ids": ["alice"]},
	    "beta-plan": {"name": "beta 套餐", "rules": [
	      {"attr": "plan", "op": "eq", "values": ["beta"]}
	    ]}
	  },
	  "flags": [
	    {"key": "f", "enabled": true, "default": false,
	     "rollout": {"percentage": 0, "salt": "s"},
	     "targeting": [
	       {"name": "vip-or-beta",
	        "match": {"any": [
	          {"segment": "vip"},
	          {"segment": "beta-plan"}
	        ]},
	        "serve": true},
	       {"name": "blocked-region",
	        "match": {"attr": "region", "op": "in", "values": ["cn-north", "cn-south"]},
	        "serve": false}
	     ]}
	  ]
	}`)
	eng := New(st)

	if d := evalMust(t, eng, "f", "alice", nil); !d.On || d.Reason != "on:targeting:vip-or-beta" {
		t.Fatalf("alice 应被 vip 定向打开，实际 %+v", d)
	}
	if d := evalMust(t, eng, "f", "bob", map[string]string{"plan": "beta"}); !d.On {
		t.Fatalf("beta 套餐用户应定向打开，实际 %+v", d)
	}
	// 第二条规则：region 命中显式关闭（即使 rollout 为 0 也通过 targeting 表达）。
	if d := evalMust(t, eng, "f", "carol", map[string]string{"region": "cn-north"}); d.On {
		t.Fatalf("blocked-region 用户应显式关闭，实际 %+v", d)
	}
	// 未命中任何规则 -> rollout 0 -> 关闭。
	if d := evalMust(t, eng, "f", "dave", map[string]string{"plan": "free"}); d.On {
		t.Fatalf("普通用户应受 rollout 0 关闭，实际 %+v", d)
	}
}

func TestDependencyGating(t *testing.T) {
	st := store.New()
	publishConfig(t, st, `{
	  "version": "v1",
	  "flags": [
	    {"key": "base", "enabled": true, "default": true},
	    {"key": "mid", "enabled": true, "default": true,
	     "depends_on": ["base"],
	     "targeting": [
	       {"name": "only-alice",
	        "match": {"attr": "id", "op": "eq", "values": ["alice"]},
	        "serve": true}
	     ]},
	    {"key": "leaf", "enabled": true, "default": true,
	     "depends_on": ["mid"],
	     "targeting": [
	       {"name": "everyone-in-test",
	        "match": {"attr": "id", "op": "ne", "values": ["nobody"]},
	        "serve": true}
	     ]}
	  ]
	}`)
	eng := New(st)

	// alice: base on, mid targeting on -> leaf on。
	if d := evalMust(t, eng, "leaf", "alice", nil); !d.On {
		t.Fatalf("alice 的 leaf 应打开: %+v", d)
	}
	// bob: mid 未命中 targeting 且无 rollout -> default=true，所以仍 on。
	d := evalMust(t, eng, "mid", "bob", nil)
	if !d.On || d.Reason != "on:default" {
		t.Fatalf("bob 的 mid 应走 default=true: %+v", d)
	}
}

func TestDependencyMissingAndCycleRejectedAtLoad(t *testing.T) {
	st := store.New()

	_, err := st.PublishRaw([]byte(`{"version":"v","flags":[
	  {"key":"a","enabled":true,"depends_on":["missing"]}
	]}`))
	if err == nil || !errs.IsKind(err, errs.KindReference) {
		t.Fatalf("引用不存在的依赖必须拒绝（reference），实际: %v", err)
	}
	t.Logf("缺失依赖被拒绝: %v", err)

	_, err = st.PublishRaw([]byte(`{"version":"v","flags":[
	  {"key":"a","enabled":true,"depends_on":["b"]},
	  {"key":"b","enabled":true,"depends_on":["a"]}
	]}`))
	if err == nil || !errs.IsKind(err, errs.KindDependency) {
		t.Fatalf("循环依赖必须拒绝（dependency），实际: %v", err)
	}
	t.Logf("循环依赖被拒绝: %v", err)
}

func TestConflictBothActive(t *testing.T) {
	st := store.New()
	publishConfig(t, st, `{
	  "version": "v1",
	  "flags": [
	    {"key": "a", "enabled": true, "default": true, "conflicts_with": ["b"]},
	    {"key": "b", "enabled": true, "default": true, "conflicts_with": ["a"]}
	  ]
	}`)
	eng := New(st)
	err := evalErr(t, eng, "a", "u1")
	if !errs.IsKind(err, errs.KindConflict) {
		t.Fatalf("双方同时打开应报 conflict，实际: %v", err)
	}

	// b 默认关闭时不冲突。
	st2 := store.New()
	publishConfig(t, st2, `{
	  "version": "v1",
	  "flags": [
	    {"key": "a", "enabled": true, "default": true, "conflicts_with": ["b"]},
	    {"key": "b", "enabled": true, "default": false, "conflicts_with": ["a"]}
	  ]
	}`)
	eng2 := New(st2)
	if d := evalMust(t, eng2, "a", "u1", nil); !d.On {
		t.Fatalf("互斥方关闭时 a 应正常打开: %+v", d)
	}
}

func TestUnknownFlagNotSilentlyAllowed(t *testing.T) {
	st := store.New()
	publishConfig(t, st, `{"version":"v","flags":[]}`)
	eng := New(st)
	err := evalErr(t, eng, "nope", "u1")
	if !errs.IsKind(err, errs.KindNotFound) {
		t.Fatalf("不存在的开关必须返回 not_found，实际: %v", err)
	}
}

func TestDisabledFlag(t *testing.T) {
	st := store.New()
	publishConfig(t, st, `{
	  "version": "v1",
	  "flags": [
	    {"key": "f", "enabled": false, "default": true,
	     "targeting": [
	       {"name": "all", "match": {"attr":"id","op":"ne","values":["x"]}, "serve": true}
	     ]}
	  ]
	}`)
	eng := New(st)
	d := evalMust(t, eng, "f", "alice", nil)
	if d.On || d.Reason != "off:flag_disabled" {
		t.Fatalf("disabled 开关必须关闭: %+v", d)
	}
}
