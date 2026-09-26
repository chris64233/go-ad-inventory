// Package clock 提供统一的当前时间来源。
//
// 所有日期边界判定与凭证过期判断都必须经过 Clock，生产环境使用 System，
// 测试中使用 Fake 做确定性的时间推进（包括模拟并发竞争）。
package clock

import (
	"sync"
	"time"
)

// Clock 抽象当前时间。
type Clock interface {
	Now() time.Time
}

// System 返回系统墙上时钟。
type System struct{}

// Now 返回当前本地时间。
func (System) Now() time.Time { return time.Now() }

// Fake 是可手动推进的测试时钟。Now 返回 t 的拷贝，调用方无法改写内部状态。
type Fake struct {
	mu sync.RWMutex
	t  time.Time
}

// NewFake 以 t 为初始时间构造 Fake 时钟。
func NewFake(t time.Time) *Fake {
	return &Fake{t: t}
}

// Now 返回当前伪造时间的副本（位置信息保留）。
func (f *Fake) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.t
}

// Set 直接设定时间。
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	f.t = t
	f.mu.Unlock()
}

// Advance 将时钟向前推进 d。
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

// DateKey 按给定位置（时区）返回 t 所在自然日的键，格式 "2006-01-02"。
// 日预算的归属与边界统一由本函数判定。
func DateKey(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("2006-01-02")
}

// DayStart 返回 t 所在自然日零点（用于计算凭证失效时间等）。
func DayStart(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	tt := t.In(loc)
	return time.Date(tt.Year(), tt.Month(), tt.Day(), 0, 0, 0, 0, loc)
}
