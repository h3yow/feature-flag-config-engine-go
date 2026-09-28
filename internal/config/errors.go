package config

import (
	"errors"
	"fmt"
)

// ErrorKind classifies why a configuration was rejected. Callers can
// assert *config.Error and switch on Kind to distinguish syntax
// problems, missing fields, illegal values, bad references, cycles and
// conflict-graph inconsistencies.
type ErrorKind int

const (
	// KindSyntax: the document is not syntactically valid JSON.
	KindSyntax ErrorKind = iota
	// KindMissingField: a required field is absent or empty.
	KindMissingField
	// KindInvalidValue: a field has a value outside its legal domain.
	KindInvalidValue
	// KindReference: a flag references an unknown audience or flag.
	KindReference
	// KindCycle: the dependency graph contains a cycle.
	KindCycle
	// KindConflict: the conflict graph is not symmetric/consistent.
	KindConflict
)

// String renders the machine readable kind.
func (k ErrorKind) String() string {
	switch k {
	case KindSyntax:
		return "syntax"
	case KindMissingField:
		return "missing_field"
	case KindInvalidValue:
		return "invalid_value"
	case KindReference:
		return "bad_reference"
	case KindCycle:
		return "dependency_cycle"
	case KindConflict:
		return "conflict"
	default:
		return "unknown"
	}
}

// Error is a classified configuration error. Path uses JSON-pointer-ish
// notation such as "flags.checkout.percentage".
type Error struct {
	Kind    ErrorKind
	Path    string
	Message string
}

func (e *Error) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.Kind, e.Message)
	}
	return fmt.Sprintf("%s at %s: %s", e.Kind, e.Path, e.Message)
}

// AsError unwraps an error chain to *Error.
func AsError(err error) (*Error, bool) {
	var ce *Error
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

func syntaxError(format string, args ...any) *Error {
	return &Error{Kind: KindSyntax, Message: fmt.Sprintf(format, args...)}
}

func missingFieldError(path string) *Error {
	return &Error{Kind: KindMissingField, Path: path, Message: "required field is missing or empty"}
}

func invalidValueError(path, msg string) *Error {
	return &Error{Kind: KindInvalidValue, Path: path, Message: msg}
}

func referenceError(path, msg string) *Error {
	return &Error{Kind: KindReference, Path: path, Message: msg}
}

func cycleError(cycle []string) *Error {
	return &Error{Kind: KindCycle, Path: "flags", Message: fmt.Sprintf("dependency cycle: %v", cycle)}
}

func conflictError(path, msg string) *Error {
	return &Error{Kind: KindConflict, Path: path, Message: msg}
}
