package adinventory

import "time"

// Clock 是服务唯一的当前时间来源，日预算的自然日边界、凭证过期判断
// 都通过它获取时间，测试中可注入固定时钟。
type Clock interface {
	Now() time.Time
}

// SystemClock 使用系统本地墙钟。
type SystemClock struct{}

// Now 返回当前时间（UTC）。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

type mockClock struct{ t time.Time }

func (c *mockClock) Now() time.Time { return c.t }

func (c *mockClock) advance(d time.Duration) { c.t = c.t.Add(d) }
