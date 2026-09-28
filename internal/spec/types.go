package spec

// Config 是一份特性开关配置的完整结构，校验通过后整体生效。
type Config struct {
	Version  string             `json:"version"`
	Flags    []Flag             `json:"flags"`
	Segments map[string]Segment `json:"segments"`
}

// Flag 描述单个特性开关。
type Flag struct {
	Key           string          `json:"key"`
	Description   string          `json:"description"`
	Enabled       bool            `json:"enabled"`
	Default       bool            `json:"default"`
	Rollout       *Rollout        `json:"rollout"`
	Targeting     []TargetingRule `json:"targeting"`
	DependsOn     []string        `json:"depends_on"`
	ConflictsWith []string        `json:"conflicts_with"`
}

// Rollout 描述按用户稳定分桶的百分比放量。
type Rollout struct {
	Percentage float64 `json:"percentage"` // [0,100]
	Salt       string  `json:"salt"`
}

// TargetingRule 是按人群/条件覆盖的规则，命中时返回 Serve。
type TargetingRule struct {
	Name  string    `json:"name"`
	Match Condition `json:"match"`
	Serve *bool     `json:"serve"`
}

// Condition 是条件树节点：All/Any 组合，或 Segment/Attr 叶子。
type Condition struct {
	All     []Condition `json:"all"`
	Any     []Condition `json:"any"`
	Segment string      `json:"segment"`
	Attr    string      `json:"attr"`
	Op      string      `json:"op"` // eq | ne | in | not_in
	Values  []string    `json:"values"`
}

// Segment 描述可被规则引用的人群。
type Segment struct {
	Name    string      `json:"name"`
	UserIDs []string    `json:"user_ids"`
	Rules   []Condition `json:"rules"`
}
