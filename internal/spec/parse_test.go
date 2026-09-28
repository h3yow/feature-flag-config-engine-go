package spec

import (
	"testing"

	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
)

func TestParseJSONClassification(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		kind errs.Kind
		ok   bool
	}{
		{"合法", `{"version":"v1","flags":[]}`, "", true},
		{"空输入", `   `, errs.KindSyntax, false},
		{"语法错误", `{"version":}`, errs.KindSyntax, false},
		{"尾部多余数据", `{"version":"v1"}{"version":"v2"}`, errs.KindSyntax, false},
		{"类型不匹配", `{"version":123,"flags":[]}`, errs.KindInvalidValue, false},
		{"未知字段", `{"version":"v1","flags":[],"unknown":1}`, errs.KindInvalidValue, false},
		{"flags 类型错", `{"version":"v1","flags":{}}`, errs.KindInvalidValue, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseJSON([]byte(tc.raw))
			t.Logf("输入: %s => err=%v", tc.raw, err)
			if tc.ok {
				if err != nil || c == nil {
					t.Fatalf("应解析成功，实际 err=%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应解析失败")
			}
			if !errs.IsKind(err, tc.kind) {
				t.Fatalf("期望分类 %s，实际: %v", tc.kind, err)
			}
		})
	}
}
