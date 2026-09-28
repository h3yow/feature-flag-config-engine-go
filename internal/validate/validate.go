package validate

import (
	"errors"
	"fmt"

	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
	"github.com/h3yow/feature-flag-config-engine-go/internal/spec"
)

var validOps = map[string]bool{"eq": true, "ne": true, "in": true, "not_in": true}

// Validate 对配置做整体校验。任何一项失败都返回错误，调用方必须整体拒绝该版本。
//
// 可能返回的错误分类：
//   - missing_field: version/key/serve/条件必填项缺失
//   - invalid_value: 重复 key、百分比越界、非法 op、条件结构错误等
//   - reference:     引用了不存在的开关或人群
//   - dependency:    depends_on 存在环（含自依赖）
//   - conflict:      互斥声明不对称、自互斥、既依赖又互斥
func Validate(c *spec.Config) error {
	v := &validator{segments: map[string]bool{}}
	for name := range c.Segments {
		v.segments[name] = true
	}
	v.run(c)
	return errors.Join(v.errs...)
}

type validator struct {
	errs     []error
	segments map[string]bool
}

func (v *validator) add(err error) { v.errs = append(v.errs, err) }

func (v *validator) run(c *spec.Config) {
	if c.Version == "" {
		v.add(errs.New(errs.KindMissingField, "version_required", "version", "缺少必填字段 version"))
	}

	seenFlags := map[string]int{}
	for i := range c.Flags {
		f := &c.Flags[i]
		p := fmt.Sprintf("flags[%d]", i)
		if f.Key == "" {
			v.add(errs.New(errs.KindMissingField, "flag_key_required", p+".key", "开关缺少必填字段 key"))
		} else if first, ok := seenFlags[f.Key]; ok {
			v.add(errs.New(errs.KindInvalidValue, "duplicate_flag", p+".key",
				fmt.Sprintf("开关 key %q 与 flags[%d] 重复", f.Key, first)))
		} else {
			seenFlags[f.Key] = i
		}

		if f.Rollout != nil {
			rp := p + ".rollout.percentage"
			if f.Rollout.Percentage < 0 || f.Rollout.Percentage > 100 {
				v.add(errs.New(errs.KindInvalidValue, "percentage_out_of_range", rp,
					fmt.Sprintf("百分比 %.4f 超出 [0,100]", f.Rollout.Percentage)))
			}
			if f.Rollout.Salt == "" {
				// salt 非强制，但空 salt 会让不同开关的分桶边界完全对齐，这里作为非法取值提示。
				v.add(errs.New(errs.KindInvalidValue, "rollout_salt_empty", p+".rollout.salt",
					"rollout.salt 不能为空（保证不同开关相互独立分桶）"))
			}
		}

		if dup := findDup(f.DependsOn); dup != "" {
			v.add(errs.New(errs.KindInvalidValue, "duplicate_dependency", p+".depends_on",
				fmt.Sprintf("依赖 %q 重复声明", dup)))
		}
		if dup := findDup(f.ConflictsWith); dup != "" {
			v.add(errs.New(errs.KindInvalidValue, "duplicate_conflict", p+".conflicts_with",
				fmt.Sprintf("互斥项 %q 重复声明", dup)))
		}
		for _, dep := range f.DependsOn {
			if dep == f.Key {
				v.add(errs.New(errs.KindDependency, "self_dependency", p+".depends_on",
					fmt.Sprintf("开关 %q 不能依赖自身", f.Key)))
			}
			for _, cf := range f.ConflictsWith {
				if dep == cf {
					v.add(errs.New(errs.KindConflict, "depend_and_conflict", p,
						fmt.Sprintf("开关 %q 对 %q 同时声明依赖与互斥", f.Key, dep)))
				}
			}
		}
		for _, cf := range f.ConflictsWith {
			if cf == f.Key {
				v.add(errs.New(errs.KindConflict, "self_conflict", p+".conflicts_with",
					fmt.Sprintf("开关 %q 不能与自身互斥", f.Key)))
			}
		}

		for j := range f.Targeting {
			r := &f.Targeting[j]
			rp := fmt.Sprintf("%s.targeting[%d]", p, j)
			if r.Serve == nil {
				v.add(errs.New(errs.KindMissingField, "serve_required", rp+".serve",
					"定向规则缺少必填字段 serve"))
			}
			v.validateCond(&r.Match, rp+".match", false)
		}
	}

	// 人群定义内部规则校验（不允许在人群内再引用人群，避免嵌套环）。
	for name, seg := range c.Segments {
		for j := range seg.Rules {
			v.validateCond(&seg.Rules[j], fmt.Sprintf("segments[%q].rules[%d]", name, j), true)
		}
	}

	// 引用完整性：depends_on / conflicts_with 必须指向存在的开关。
	for i := range c.Flags {
		f := &c.Flags[i]
		p := fmt.Sprintf("flags[%d]", i)
		for j, dep := range f.DependsOn {
			if dep != f.Key && seenFlags[dep] == 0 && !flagExists(seenFlags, dep) {
				v.add(errs.New(errs.KindReference, "missing_dependency",
					fmt.Sprintf("%s.depends_on[%d]", p, j),
					fmt.Sprintf("开关 %q 依赖了不存在的开关 %q", f.Key, dep)))
			}
		}
		for j, cf := range f.ConflictsWith {
			if cf != f.Key && !flagExists(seenFlags, cf) {
				v.add(errs.New(errs.KindReference, "missing_conflict_target",
					fmt.Sprintf("%s.conflicts_with[%d]", p, j),
					fmt.Sprintf("开关 %q 互斥声明引用了不存在的开关 %q", f.Key, cf)))
			}
		}
	}

	v.detectDepCycle(c)
	v.checkConflictSymmetry(c)
}

// flagExists 处理 key 恰好为首个开关（下标 0）的零值歧义。
func flagExists(seen map[string]int, key string) bool {
	_, ok := seen[key]
	return ok
}

func (v *validator) validateCond(c *spec.Condition, path string, insideSegment bool) {
	hasAll := len(c.All) > 0
	hasAny := len(c.Any) > 0
	hasSeg := c.Segment != ""
	hasAttr := c.Attr != "" || c.Op != "" || len(c.Values) > 0

	if hasAll && hasAny {
		v.add(errs.New(errs.KindInvalidValue, "cond_all_and_any", path, "条件不能同时包含 all 与 any"))
		return
	}
	if hasAll || hasAny {
		if hasSeg || hasAttr || c.Attr != "" || c.Op != "" {
			v.add(errs.New(errs.KindInvalidValue, "cond_branch_and_leaf", path,
				"组合条件(all/any)不能同时携带叶子字段(segment/attr/op/values)"))
			return
		}
		subs := c.All
		op := "all"
		if hasAny {
			subs, op = c.Any, "any"
		}
		for i := range subs {
			v.validateCond(&subs[i], fmt.Sprintf("%s.%s[%d]", path, op, i), insideSegment)
		}
		return
	}

	if hasSeg {
		if hasAttr || c.Attr != "" || c.Op != "" || len(c.Values) > 0 {
			v.add(errs.New(errs.KindInvalidValue, "cond_segment_and_attr", path,
				"人群条件不能同时携带 attr/op/values"))
		}
		if insideSegment {
			v.add(errs.New(errs.KindInvalidValue, "nested_segment_ref", path,
				"人群定义内部不能再引用其他人群"))
		} else if !v.segments[c.Segment] {
			v.add(errs.New(errs.KindReference, "missing_segment", path+".segment",
				fmt.Sprintf("引用了不存在的人群 %q", c.Segment)))
		}
		return
	}

	if hasAttr || c.Op != "" || len(c.Values) > 0 {
		if c.Attr == "" {
			v.add(errs.New(errs.KindMissingField, "cond_attr_required", path+".attr", "属性条件缺少 attr"))
		}
		if c.Op == "" {
			v.add(errs.New(errs.KindMissingField, "cond_op_required", path+".op", "属性条件缺少 op"))
		} else if !validOps[c.Op] {
			v.add(errs.New(errs.KindInvalidValue, "cond_bad_op", path+".op",
				fmt.Sprintf("非法操作符 %q（允许 eq/ne/in/not_in）", c.Op)))
		}
		if len(c.Values) == 0 {
			v.add(errs.New(errs.KindMissingField, "cond_values_required", path+".values", "属性条件缺少 values"))
		} else if (c.Op == "eq" || c.Op == "ne") && len(c.Values) != 1 {
			v.add(errs.New(errs.KindInvalidValue, "cond_values_count", path+".values",
				fmt.Sprintf("op %q 要求 values 恰好 1 个元素", c.Op)))
		}
		return
	}

	v.add(errs.New(errs.KindMissingField, "empty_condition", path,
		"空条件：需要 all/any、segment 或 attr 之一"))
}

func (v *validator) detectDepCycle(c *spec.Config) {
	deps := map[string][]string{}
	keys := map[string]bool{}
	for i := range c.Flags {
		keys[c.Flags[i].Key] = true
	}
	for i := range c.Flags {
		f := &c.Flags[i]
		for _, d := range f.DependsOn {
			if keys[d] {
				deps[f.Key] = append(deps[f.Key], d)
			}
		}
	}

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string

	var visit func(key string) bool
	visit = func(key string) bool {
		color[key] = gray
		stack = append(stack, key)
		for _, d := range deps[key] {
			if color[d] == gray {
				cycle := append([]string{}, stack...)
				cycle = append(cycle, d)
				v.add(errs.New(errs.KindDependency, "dependency_cycle", "flags.depends_on",
					fmt.Sprintf("依赖存在环：%v", cycle)))
				return true
			}
			if color[d] == white && visit(d) {
				return true
			}
		}
		color[key] = black
		stack = stack[:len(stack)-1]
		return false
	}

	for i := range c.Flags {
		key := c.Flags[i].Key
		if key != "" && color[key] == white {
			if visit(key) {
				return // 报告第一个环即可
			}
		}
	}
}

func (v *validator) checkConflictSymmetry(c *spec.Config) {
	byKey := map[string]*spec.Flag{}
	for i := range c.Flags {
		byKey[c.Flags[i].Key] = &c.Flags[i]
	}
	for i := range c.Flags {
		f := &c.Flags[i]
		for _, other := range f.ConflictsWith {
			if other == f.Key {
				continue // 自互斥已在前面报告
			}
			g, ok := byKey[other]
			if !ok {
				continue // 引用缺失已报告
			}
			if !contains(g.ConflictsWith, f.Key) {
				v.add(errs.New(errs.KindConflict, "asymmetric_conflict", "flags.conflicts_with",
					fmt.Sprintf("互斥声明不对称：%q 声明与 %q 互斥，但反向缺失", f.Key, other)))
			}
		}
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func findDup(list []string) string {
	seen := map[string]bool{}
	for _, x := range list {
		if seen[x] {
			return x
		}
		seen[x] = true
	}
	return ""
}
