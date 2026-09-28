package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
)

// ParseJSON 解析原始 JSON 配置，仅做语法层解析，不做语义校验。
//
// 错误分类：
//   - syntax:        JSON 语法错误、空输入、尾部多余数据
//   - invalid_value: 字段类型不匹配、出现未定义字段
func ParseJSON(raw []byte) (*Config, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errs.New(errs.KindSyntax, "empty_config", "", "配置为空")
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, classifyJSONErr(err)
	}
	// 拒绝 JSON 值之后的多余内容（如两个并列对象）。
	if dec.More() {
		return nil, errs.New(errs.KindSyntax, "trailing_data", "", "配置在首个 JSON 值之后仍有多余数据")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errs.New(errs.KindSyntax, "trailing_data", "", "配置在首个 JSON 值之后仍有多余数据")
	}
	return &c, nil
}

func classifyJSONErr(err error) error {
	switch e := err.(type) {
	case *json.SyntaxError:
		return errs.Wrap(err, errs.KindSyntax, "json_syntax", "",
			fmt.Sprintf("JSON 语法错误（偏移 %d）", e.Offset))
	case *json.UnmarshalTypeError:
		path := e.Field
		if path == "" {
			path = fmt.Sprintf("偏移 %d", e.Offset)
		}
		return errs.Wrap(err, errs.KindInvalidValue, "type_mismatch", path,
			fmt.Sprintf("字段类型错误：期望 %s，实际为 %s", e.Type.String(), e.Value))
	default:
		// DisallowUnknownFields 拒绝未知字段时走这里。
		msg := err.Error()
		path := ""
		if i := bytes.IndexByte([]byte(msg), '"'); i >= 0 {
			rest := msg[i+1:]
			if j := bytes.IndexByte([]byte(rest), '"'); j >= 0 {
				path = rest[:j]
			}
		}
		if path != "" {
			return errs.Wrap(err, errs.KindInvalidValue, "unknown_field", path, "存在未定义字段")
		}
		return errs.Wrap(err, errs.KindSyntax, "json_syntax", "", "JSON 解析失败")
	}
}
