package validate

import (
	"testing"

	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
	"github.com/h3yow/feature-flag-config-engine-go/internal/spec"
)

func bptr(b bool) *bool { return &b }

func baseConfig() *spec.Config {
	return &spec.Config{
		Version: "v1",
		Flags: []spec.Flag{
			{Key: "a", Enabled: true, Default: true},
			{Key: "b", Enabled: true, Default: false},
		},
	}
}

func expectKind(t *testing.T, c *spec.Config, kind errs.Kind) {
	t.Helper()
	err := Validate(c)
	if err == nil {
		t.Fatalf("期望 %s 错误，但校验通过", kind)
	}
	if !errs.IsKind(err, kind) {
		t.Fatalf("期望分类 %s，实际: %v", kind, err)
	}
	t.Logf("如预期分类为 %s: %v", kind, err)
}

func TestValidConfig(t *testing.T) {
	if err := Validate(baseConfig()); err != nil {
		t.Fatalf("基础配置应通过: %v", err)
	}
}

func TestMissingFields(t *testing.T) {
	c := baseConfig()
	c.Version = ""
	expectKind(t, c, errs.KindMissingField)

	c = baseConfig()
	c.Flags[0].Key = ""
	expectKind(t, c, errs.KindMissingField)

	c = baseConfig()
	c.Flags[0].Targeting = []spec.TargetingRule{
		{Name: "r", Match: spec.Condition{Attr: "x", Op: "eq", Values: []string{"1"}}},
	}
	expectKind(t, c, errs.KindMissingField) // serve 缺失
}

func TestInvalidValues(t *testing.T) {
	c := baseConfig()
	c.Flags[0].Rollout = &spec.Rollout{Percentage: 101, Salt: "s"}
	expectKind(t, c, errs.KindInvalidValue)

	c = baseConfig()
	c.Flags[0].Rollout = &spec.Rollout{Percentage: -0.1, Salt: "s"}
	expectKind(t, c, errs.KindInvalidValue)

	c = baseConfig()
	c.Flags[0].Rollout = &spec.Rollout{Percentage: 10, Salt: ""}
	expectKind(t, c, errs.KindInvalidValue)

	c = baseConfig()
	c.Flags = append(c.Flags, spec.Flag{Key: "a", Enabled: true}) // 重复 key
	expectKind(t, c, errs.KindInvalidValue)

	c = baseConfig()
	c.Flags[0].Targeting = []spec.TargetingRule{{
		Name:  "r",
		Match: spec.Condition{Attr: "x", Op: "weird", Values: []string{"1"}},
		Serve: bptr(true),
	}}
	expectKind(t, c, errs.KindInvalidValue) // 非法操作符

	c = baseConfig()
	c.Flags[0].Targeting = []spec.TargetingRule{{
		Name:  "r",
		Match: spec.Condition{Attr: "x", Op: "eq", Values: []string{"1", "2"}},
		Serve: bptr(true),
	}}
	expectKind(t, c, errs.KindInvalidValue) // eq 的 values 必须 1 个

	c = baseConfig()
	c.Flags[0].Targeting = []spec.TargetingRule{{
		Name:  "r",
		Match: spec.Condition{All: []spec.Condition{{Segment: "x"}}, Attr: "y"},
		Serve: bptr(true),
	}}
	expectKind(t, c, errs.KindInvalidValue) // 组合与叶子混用

	c = baseConfig()
	c.Flags[0].Targeting = []spec.TargetingRule{{
		Name:  "r",
		Match: spec.Condition{},
		Serve: bptr(true),
	}}
	expectKind(t, c, errs.KindMissingField) // 空条件
}

func TestReferences(t *testing.T) {
	c := baseConfig()
	c.Flags[0].DependsOn = []string{"ghost"}
	expectKind(t, c, errs.KindReference)

	c = baseConfig()
	c.Flags[0].ConflictsWith = []string{"ghost"}
	expectKind(t, c, errs.KindReference)

	c = baseConfig()
	c.Segments = map[string]spec.Segment{}
	c.Flags[0].Targeting = []spec.TargetingRule{{
		Name:  "r",
		Match: spec.Condition{Segment: "missing-seg"},
		Serve: bptr(true),
	}}
	expectKind(t, c, errs.KindReference)
}

func TestDependencyAndConflictSemantics(t *testing.T) {
	c := baseConfig()
	c.Flags[0].DependsOn = []string{"a"} // 自依赖
	expectKind(t, c, errs.KindDependency)

	c = baseConfig()
	c.Flags[0].DependsOn = []string{"b"}
	c.Flags[1].DependsOn = []string{"a"}
	expectKind(t, c, errs.KindDependency) // 环

	c = baseConfig()
	c.Flags[0].ConflictsWith = []string{"a"} // 自互斥
	expectKind(t, c, errs.KindConflict)

	c = baseConfig()
	c.Flags[0].ConflictsWith = []string{"b"} // 单向声明
	expectKind(t, c, errs.KindConflict)

	c = baseConfig()
	c.Flags[0].DependsOn = []string{"b"}
	c.Flags[0].ConflictsWith = []string{"b"} // 既依赖又互斥
	expectKind(t, c, errs.KindConflict)

	c = baseConfig()
	c.Flags[0].DependsOn = []string{"b", "b"} // 重复依赖
	expectKind(t, c, errs.KindInvalidValue)
}

func TestNestedSegmentRefRejected(t *testing.T) {
	c := baseConfig()
	c.Segments = map[string]spec.Segment{
		"s1": {Name: "s1", Rules: []spec.Condition{{Segment: "s2"}}},
		"s2": {Name: "s2"},
	}
	expectKind(t, c, errs.KindInvalidValue)
}

func TestMultipleErrorsCollected(t *testing.T) {
	c := &spec.Config{Version: ""} // 缺 version
	c.Flags = []spec.Flag{
		{Key: "", Enabled: true},
		{Key: "f", Enabled: true, Rollout: &spec.Rollout{Percentage: 200, Salt: "s"}},
	}
	err := Validate(c)
	if err == nil {
		t.Fatal("应同时报多个错误")
	}
	if !errs.IsKind(err, errs.KindMissingField) || !errs.IsKind(err, errs.KindInvalidValue) {
		t.Fatalf("错误聚合后应同时可识别两类，实际: %v", err)
	}
	t.Logf("聚合错误: %v", err)
}
