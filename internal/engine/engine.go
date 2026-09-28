package engine

import (
	"fmt"

	"github.com/h3yow/feature-flag-config-engine-go/internal/compile"
	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
	"github.com/h3yow/feature-flag-config-engine-go/internal/spec"
	"github.com/h3yow/feature-flag-config-engine-go/internal/store"
)

// User 是判定输入：稳定 ID 与字符串属性（属性 id 自动对应用户 ID）。
type User struct {
	ID    string
	Attrs map[string]string
}

// Decision 是判定结果，Reason 与 Trace 记录判定依据，便于日志审计。
type Decision struct {
	FlagKey    string   `json:"flag_key"`
	On         bool     `json:"on"`
	Reason     string   `json:"reason"`
	Revision   int64    `json:"revision"`
	Bucket     int      `json:"bucket"`
	Percentage float64  `json:"percentage"`
	Trace      []string `json:"trace"`
}

// Engine 基于 store 中的当前快照做判定，自身无任何可变状态，可并发使用。
type Engine struct {
	store *store.Store
}

func New(st *store.Store) *Engine { return &Engine{store: st} }

// gateResult 是单个开关“自身门控”的求值结果（不含互斥检测）。
type gateResult struct {
	on         bool
	reason     string
	bucket     int
	percentage float64
	trace      []string
}

// Eval 判定某用户对某开关的结果：
//
//  1. 开关不存在 -> not_found（不静默放行）
//  2. 求自身门控（enabled -> depends_on -> targeting -> rollout -> default）
//  3. 门控为 on 时检查互斥：互斥方门控也为 on 则返回 conflict 错误（不静默放行）
func (e *Engine) Eval(flagKey string, u User) (Decision, error) {
	snap := e.store.Current()
	if snap == nil {
		return Decision{}, errs.New(errs.KindState, "no_config", "", "尚未加载任何配置")
	}
	d := Decision{FlagKey: flagKey, Revision: snap.Revision}

	g, err := e.gate(snap, flagKey, u, map[string]bool{})
	d.Trace = g.trace
	if err != nil {
		return d, err
	}
	d.On = g.on
	d.Reason = g.reason
	d.Bucket = g.bucket
	d.Percentage = g.percentage

	if !g.on {
		d.Trace = append(d.Trace, fmt.Sprintf("门控关闭（%s），跳过互斥检测", g.reason))
		return d, nil
	}

	// 门控打开时检查互斥：互斥方自身门控也打开才算冲突。
	f, ok := snap.Flag(flagKey)
	if !ok {
		return d, errs.New(errs.KindNotFound, "flag_not_found", "flags."+flagKey,
			fmt.Sprintf("开关 %q 不存在", flagKey))
	}
	for _, other := range f.ConflictsWith {
		og, oerr := e.gate(snap, other, u, map[string]bool{})
		d.Trace = append(d.Trace, prefixTrace(og.trace, fmt.Sprintf("conflict-check[%s]:", other))...)
		if oerr != nil {
			return d, oerr
		}
		if og.on {
			d.Trace = append(d.Trace,
				fmt.Sprintf("互斥冲突：%q 与 %q 对用户 %q 同时打开", flagKey, other, u.ID))
			d.On = false
			d.Reason = ""
			return d, errs.New(errs.KindConflict, "flags_active_together", "flags."+flagKey,
				fmt.Sprintf("互斥开关 %q 与 %q 对用户 %q 同时打开", flagKey, other, u.ID))
		}
		d.Trace = append(d.Trace,
			fmt.Sprintf("互斥方 %q 门控关闭（%s），不冲突", other, og.reason))
	}
	return d, nil
}

// gate 计算开关自身门控。active 记录当前依赖链，防止异常环造成无限递归。
func (e *Engine) gate(snap *compile.Snapshot, flagKey string, u User, active map[string]bool) (gateResult, error) {
	var r gateResult
	add := func(format string, args ...any) {
		r.trace = append(r.trace, fmt.Sprintf(format, args...))
	}

	f, ok := snap.Flag(flagKey)
	if !ok {
		add("flag %q 不存在", flagKey)
		return r, errs.New(errs.KindNotFound, "flag_not_found", "flags."+flagKey,
			fmt.Sprintf("开关 %q 不存在", flagKey))
	}
	add("revision=%d flag=%q user=%q", snap.Revision, flagKey, u.ID)

	if !f.Enabled {
		add("flag disabled（enabled=false）")
		r.reason = "off:flag_disabled"
		return r, nil
	}

	// 依赖：按拓扑序求值，任一依赖未打开则本开关关闭。
	for _, dep := range snap.DepsOf(flagKey) {
		if active[dep] {
			continue
		}
		sub := cloneSet(active)
		sub[flagKey] = true
		if len(sub) > 64 {
			return r, errs.New(errs.KindDependency, "dependency_too_deep",
				"flags."+flagKey, "依赖链异常过深（可能存在未在加载期拦截的环）")
		}
		dg, derr := e.gate(snap, dep, u, sub)
		r.trace = append(r.trace, prefixTrace(dg.trace, fmt.Sprintf("dep[%s]:", dep))...)
		if derr != nil {
			return r, derr
		}
		if !dg.on {
			add("依赖 %q 未打开（%s），本开关关闭", dep, dg.reason)
			r.reason = "off:dependency:" + dep
			return r, nil
		}
		add("依赖 %q 已打开", dep)
	}

	// 定向规则：按顺序，第一条命中即生效。
	for i := range f.Targeting {
		rl := &f.Targeting[i]
		matched, mtrace := matchCond(snap, &rl.Match, u)
		r.trace = append(r.trace, prefixTrace(mtrace, fmt.Sprintf("targeting[%s]:", ruleName(rl, i)))...)
		if matched {
			r.on = *rl.Serve
			r.reason = fmt.Sprintf("%s:targeting:%s", onOff(r.on), ruleName(rl, i))
			add("targeting[%s] 命中，serve=%v", ruleName(rl, i), r.on)
			return r, nil
		}
		add("targeting[%s] 未命中", ruleName(rl, i))
	}

	// 百分比灰度：桶号 [0, pct) 为 on。
	if f.Rollout != nil {
		r.bucket = bucket(f.Rollout.Salt, flagKey, u.ID)
		r.percentage = f.Rollout.Percentage
		r.on = r.bucket < int(r.percentage)
		r.reason = onOff(r.on) + ":rollout"
		add("rollout salt=%q bucket=%d percentage=%v => %v",
			f.Rollout.Salt, r.bucket, r.percentage, r.on)
		return r, nil
	}

	r.on = f.Default
	r.reason = onOff(r.on) + ":default"
	add("使用 default=%v", f.Default)
	return r, nil
}

func matchCond(snap *compile.Snapshot, c *spec.Condition, u User) (bool, []string) {
	if len(c.All) > 0 {
		var tr []string
		for i := range c.All {
			ok, sub := matchCond(snap, &c.All[i], u)
			tr = append(tr, prefixTrace(sub, fmt.Sprintf("all[%d]:", i))...)
			if !ok {
				tr = append(tr, "all => false（短路）")
				return false, tr
			}
		}
		tr = append(tr, "all => true")
		return true, tr
	}
	if len(c.Any) > 0 {
		var tr []string
		for i := range c.Any {
			ok, sub := matchCond(snap, &c.Any[i], u)
			tr = append(tr, prefixTrace(sub, fmt.Sprintf("any[%d]:", i))...)
			if ok {
				tr = append(tr, "any => true（短路）")
				return true, tr
			}
		}
		tr = append(tr, "any => false")
		return false, tr
	}
	if c.Segment != "" {
		return matchSegment(snap, c.Segment, u)
	}
	return matchAttr(c, u)
}

func matchSegment(snap *compile.Snapshot, name string, u User) (bool, []string) {
	seg, ok := snap.Segment(name)
	if !ok {
		return false, []string{fmt.Sprintf("segment %q 不存在 => false", name)}
	}
	for _, id := range seg.UserIDs {
		if id == u.ID {
			return true, []string{fmt.Sprintf("segment %q user_ids 命中 %q", name, u.ID)}
		}
	}
	var tr []string
	for i := range seg.Rules {
		ok, sub := matchCond(snap, &seg.Rules[i], u)
		tr = append(tr, prefixTrace(sub, fmt.Sprintf("segment %q rule[%d]:", name, i))...)
		if ok {
			return true, tr
		}
	}
	tr = append(tr, fmt.Sprintf("segment %q => false", name))
	return false, tr
}

func matchAttr(c *spec.Condition, u User) (bool, []string) {
	val := u.Attrs[c.Attr]
	if c.Attr == "id" {
		val = u.ID
	}
	var hit bool
	switch c.Op {
	case "eq", "in":
		for _, v := range c.Values {
			if v == val {
				hit = true
				break
			}
		}
	case "ne":
		hit = val != c.Values[0]
	case "not_in":
		hit = true
		for _, v := range c.Values {
			if v == val {
				hit = false
				break
			}
		}
	default:
		hit = false
	}
	return hit, []string{fmt.Sprintf("attr %s %s %v => %v（actual=%q）",
		c.Attr, c.Op, c.Values, hit, val)}
}

func ruleName(r *spec.TargetingRule, i int) string {
	if r.Name != "" {
		return r.Name
	}
	return fmt.Sprintf("#%d", i)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func prefixTrace(trace []string, prefix string) []string {
	out := make([]string, len(trace))
	for i, t := range trace {
		out[i] = prefix + t
	}
	return out
}

func cloneSet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}
