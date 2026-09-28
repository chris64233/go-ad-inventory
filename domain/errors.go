// Package domain 实现广告预算的预占（reserve）、核销（capture）、
// 取消（cancel）与过期（expire）核心领域逻辑。
//
// 所有状态变更都以事件形式持久化，内存态由事件重放得到；
// 服务内部用单一互斥锁把“校验—扣减—落事件”变成原子操作，
// 保证并发争抢时总预算与日预算都不会被突破。
package domain

import (
	"errors"
	"fmt"

	"github.com/chris64233/go-ad-inventory/money"
)

// ErrorCode 区分调用方可识别的错误类别。
type ErrorCode string

const (
	// CodeInvalidArgument 参数错误（400）。
	CodeInvalidArgument ErrorCode = "invalid_argument"
	// CodeNotFound 活动或凭证不存在（404）。
	CodeNotFound ErrorCode = "not_found"
	// CodeBudgetExceeded 总预算、日预算或时段节奏额度不足（422）。
	CodeBudgetExceeded ErrorCode = "budget_exceeded"
	// CodeConflict 状态冲突：凭证已终态、迟到取消、失效凭证收到回执等（409）。
	CodeConflict ErrorCode = "conflict"
	// CodeIdempotencyConflict 幂等冲突：同一请求号/回执号/调整号但内容变化（409）。
	CodeIdempotencyConflict ErrorCode = "idempotency_conflict"
	// CodeVersionConflict 配置版本冲突：expected_version 与当前版本不一致（409）。
	CodeVersionConflict ErrorCode = "version_conflict"
)

// 各类别的哨兵错误，配合 errors.Is 使用。
var (
	ErrInvalidArgument     = &Error{Code: CodeInvalidArgument}
	ErrNotFound            = &Error{Code: CodeNotFound}
	ErrBudgetExceeded      = &Error{Code: CodeBudgetExceeded}
	ErrConflict            = &Error{Code: CodeConflict}
	ErrIdempotencyConflict = &Error{Code: CodeIdempotencyConflict}
	ErrVersionConflict     = &Error{Code: CodeVersionConflict}
)

// Error 是领域层统一错误类型，携带机器可读 Code 与人类可读信息。
type Error struct {
	Code    ErrorCode
	Op      string // 发生错误的操作，如 "Reserve"
	Message string
	// Level 仅预算不足时有意义："total"、"daily" 或 "slot"。
	Level string
	// DayKey 在日/时段预算冲突时填写涉及的自然日。
	DayKey string
	// Requested / Available 仅预算不足时填写。
	Requested money.Money
	Available money.Money
	// CurrentVersion 仅版本冲突时填写：活动当前配置版本。
	CurrentVersion int64
	// Limit / Committed 仅预算调整被整体拒绝时填写：
	// Limit 是试图设置的新上限，Committed 是该上限不得低于的已占用金额
	// （已核销额，或已核销额+有效预占）。
	Limit     money.Money
	Committed money.Money
	cause     error
}

func (e *Error) Error() string {
	s := string(e.Code)
	if e.Op != "" {
		s += " at " + e.Op
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Unwrap 支持嵌套原因。
func (e *Error) Unwrap() error { return e.cause }

// Is 使任意同 Code 的 *Error 互相匹配哨兵。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

func eInvalid(op, format string, args ...any) error {
	return &Error{Code: CodeInvalidArgument, Op: op, Message: fmt.Sprintf(format, args...)}
}

func eNotFound(op, format string, args ...any) error {
	return &Error{Code: CodeNotFound, Op: op, Message: fmt.Sprintf(format, args...)}
}

func eConflict(op, format string, args ...any) error {
	return &Error{Code: CodeConflict, Op: op, Message: fmt.Sprintf(format, args...)}
}

func eIdemConflict(op, format string, args ...any) error {
	return &Error{Code: CodeIdempotencyConflict, Op: op, Message: fmt.Sprintf(format, args...)}
}

func eBudget(op, level string, requested, available money.Money) error {
	return &Error{
		Code:      CodeBudgetExceeded,
		Op:        op,
		Level:     level,
		Requested: requested,
		Available: available,
		Message: fmt.Sprintf("%s budget insufficient: requested %s, available %s",
			level, requested.String(), available.String()),
	}
}

// eVersionConflict 构造版本冲突错误，带活动当前版本。
func eVersionConflict(op string, expected, current int64) error {
	return &Error{
		Code:           CodeVersionConflict,
		Op:             op,
		CurrentVersion: current,
		Message: fmt.Sprintf("config version conflict: expected %d, current is %d",
			expected, current),
	}
}

// eAdjustRejected 构造总预算调整被整体拒绝的错误。
func eAdjustRejected(op string, limit, committed money.Money) error {
	return &Error{
		Code:      CodeBudgetExceeded,
		Op:        op,
		Level:     "total",
		Limit:     limit,
		Committed: committed,
		Message: fmt.Sprintf("cannot lower total budget to %s: committed amount is %s "+
			"(adjustment rejected as a whole, no reservation cancelled)",
			limit.String(), committed.String()),
	}
}

// eDailyAdjustRejected 构造日预算调整被整体拒绝的错误，带涉及的自然日。
func eDailyAdjustRejected(op, dayKey string, limit, committed money.Money) error {
	return &Error{
		Code:      CodeBudgetExceeded,
		Op:        op,
		Level:     "daily",
		DayKey:    dayKey,
		Limit:     limit,
		Committed: committed,
		Message: fmt.Sprintf("cannot lower daily budget to %s: committed amount on %s is %s "+
			"(adjustment rejected as a whole, no reservation cancelled)",
			limit.String(), dayKey, committed.String()),
	}
}

// AsError 从任意错误中提取领域 *Error。
func AsError(err error) (*Error, bool) {
	var de *Error
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}
