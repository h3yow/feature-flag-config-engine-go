package compile

import (
	"time"

	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
	"github.com/h3yow/feature-flag-config-engine-go/internal/spec"
	"github.com/h3yow/feature-flag-config-engine-go/internal/validate"
)

// Snapshot 是一份不可变的已编译配置快照，判定期间直接读取指针，无需加锁。
type Snapshot struct {
	Config     *spec.Config
	Revision   int64
	ContentSHA string
	CreatedAt  time.Time

	flags    map[string]*spec.Flag
	segments map[string]*spec.Segment
	order    []string // depends_on 的拓扑序（被依赖者在前）
}

// Compile 校验并编译配置。入参配置不应再被外部修改。
func Compile(c *spec.Config) (*Snapshot, error) {
	if err := validate.Validate(c); err != nil {
		return nil, err
	}
	s := &Snapshot{
		Config:    c,
		CreatedAt: time.Now().UTC(),
		flags:     make(map[string]*spec.Flag, len(c.Flags)),
		segments:  make(map[string]*spec.Segment, len(c.Segments)),
	}
	for i := range c.Flags {
		f := &c.Flags[i]
		s.flags[f.Key] = f
	}
	for name := range c.Segments {
		seg := c.Segments[name]
		s.segments[name] = &seg
	}

	order, err := topoOrder(s.flags)
	if err != nil {
		return nil, err
	}
	s.order = order
	return s, nil
}

func (s *Snapshot) Flag(key string) (*spec.Flag, bool) {
	f, ok := s.flags[key]
	return f, ok
}

func (s *Snapshot) Segment(key string) (*spec.Segment, bool) {
	seg, ok := s.segments[key]
	return seg, ok
}

func (s *Snapshot) FlagKeys() []string {
	out := make([]string, 0, len(s.flags))
	for k := range s.flags {
		out = append(out, k)
	}
	return out
}

// DepsOf 返回某开关声明的依赖（以拓扑序排列）。
func (s *Snapshot) DepsOf(key string) []string {
	f, ok := s.flags[key]
	if !ok {
		return nil
	}
	set := map[string]bool{}
	for _, d := range f.DependsOn {
		set[d] = true
	}
	out := make([]string, 0, len(set))
	for _, k := range s.order {
		if set[k] {
			out = append(out, k)
		}
	}
	return out
}

// topoOrder 基于入度做 Kahn 拓扑排序；能进入这里说明配置已通过环检测，
// 这里再防御性处理一次，发现环则返回 dependency 错误。
func topoOrder(flags map[string]*spec.Flag) ([]string, error) {
	indeg := make(map[string]int, len(flags))
	for k := range flags {
		indeg[k] = 0
	}
	for k, f := range flags {
		seen := map[string]bool{}
		for _, d := range f.DependsOn {
			if _, ok := flags[d]; !ok || seen[d] {
				continue
			}
			seen[d] = true
			indeg[k]++
		}
	}

	// 按 key 排序入队，保证同一份配置编译结果确定。
	var queue []string
	for k := range flags {
		if indeg[k] == 0 {
			queue = append(queue, k)
		}
	}
	sortStrings(queue)

	var order []string
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		order = append(order, k)
		var newly []string
		for other, f := range flags {
			depends := false
			for _, d := range f.DependsOn {
				if d == k {
					depends = true
					break
				}
			}
			if depends {
				indeg[other]--
				if indeg[other] == 0 {
					newly = append(newly, other)
				}
			}
		}
		sortStrings(newly)
		queue = append(queue, newly...)
	}

	if len(order) != len(flags) {
		return nil, errs.New(errs.KindDependency, "dependency_cycle", "flags.depends_on",
			"编译阶段检测到依赖环")
	}
	return order, nil
}

func sortStrings(s []string) {
	// 小排序，避免为单处用途引入 sort 包的间接依赖混乱。
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
