package adinventory

import "fmt"

// ErrorKind 区分业务错误类型，便于接口层映射为不同的状态码与错误码。
type ErrorKind string

const (
	// KindInvalidArgument 参数错误（金额非法、字段缺失等）。
	KindInvalidArgument ErrorKind = "invalid_argument"
	// KindNotFound 资源不存在。
	KindNotFound ErrorKind = "not_found"
	// KindBudgetExceeded 预算不足（总预算或当日日预算）。
	KindBudgetExceeded ErrorKind = "budget_exceeded"
	// KindStateConflict 状态冲突（凭证已终态、迟到取消/回执等）。
	KindStateConflict ErrorKind = "state_conflict"
	// KindIdempotencyConflict 幂等冲突：编号相同但内容不一致。
	KindIdempotencyConflict ErrorKind = "idempotency_conflict"
)

// Error 是服务对外返回的统一业务错误类型。
type Error struct {
	Kind  ErrorKind
	Scope string // 仅 KindBudgetExceeded 使用："total" 或 "daily"
	Msg   string
}

func (e *Error) Error() string {
	if e.Scope != "" {
		return fmt.Sprintf("adinventory: %s (%s): %s", e.Kind, e.Scope, e.Msg)
	}
	return fmt.Sprintf("adinventory: %s: %s", e.Kind, e.Msg)
}

// AsError 从 err 中提取 *Error。
func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	e, ok := err.(*Error)
	return e, ok
}

func invalidArgumentf(format string, args ...any) error {
	return &Error{Kind: KindInvalidArgument, Msg: fmt.Sprintf(format, args...)}
}

func notFoundf(format string, args ...any) error {
	return &Error{Kind: KindNotFound, Msg: fmt.Sprintf(format, args...)}
}

func budgetExceededf(scope, format string, args ...any) error {
	return &Error{Kind: KindBudgetExceeded, Scope: scope, Msg: fmt.Sprintf(format, args...)}
}

func stateConflictf(format string, args ...any) error {
	return &Error{Kind: KindStateConflict, Msg: fmt.Sprintf(format, args...)}
}

func idempotencyConflictf(format string, args ...any) error {
	return &Error{Kind: KindIdempotencyConflict, Msg: fmt.Sprintf(format, args...)}
}
