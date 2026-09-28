package errs

import "fmt"

// Kind 对配置与判定错误进行分类。
type Kind string

const (
	KindSyntax       Kind = "syntax"        // 语法问题（JSON 解析失败）
	KindMissingField Kind = "missing_field" // 必填字段缺失
	KindInvalidValue Kind = "invalid_value" // 取值非法
	KindReference    Kind = "reference"     // 引用了不存在的对象
	KindDependency   Kind = "dependency"    // 依赖问题（如循环依赖）
	KindConflict     Kind = "conflict"      // 互斥冲突
	KindNotFound     Kind = "not_found"     // 运行时对象不存在
	KindState        Kind = "state"         // 状态问题（如无版本可回滚）
)

// Error 是本系统统一的错误类型，携带分类、机器可读码、配置路径与描述。
type Error struct {
	Kind Kind
	Code string
	Path string
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %s", e.Kind, e.Msg)
	if e.Path != "" {
		s = fmt.Sprintf("%s (%s)", s, e.Path)
	}
	if e.Err != nil {
		s = fmt.Sprintf("%s: %v", s, e.Err)
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

func New(kind Kind, code, path, msg string) *Error {
	return &Error{Kind: kind, Code: code, Path: path, Msg: msg}
}

func Wrap(err error, kind Kind, code, path, msg string) *Error {
	return &Error{Kind: kind, Code: code, Path: path, Msg: msg, Err: err}
}

// IsKind 判断错误链中是否存在指定分类的错误（可穿透 errors.Join 的多错误聚合）。
func IsKind(err error, kind Kind) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*Error); ok && e.Kind == kind {
		return true
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		for _, sub := range u.Unwrap() {
			if IsKind(sub, kind) {
				return true
			}
		}
		return false
	}
	if u, ok := err.(interface{ Unwrap() error }); ok {
		return IsKind(u.Unwrap(), kind)
	}
	return false
}
