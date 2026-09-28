package domain

import (
	"fmt"
	"math/big"
	"time"

	"github.com/chris64233/go-ad-inventory/money"
)

// SlotsPerDay 是投放曲线的时段数：按小时把一天切为 24 个时段。
const SlotsPerDay = 24

// Curve 是每日投放曲线：24 个非负整数权重，权重越大表示对应时段解锁的额度越多。
//
// 时段 h 的“累计目标” = ceil(dailyCap * prefix[h] / sum(weights))，
// 即到该时段结束时允许累计达到的消耗上限。权重为 0 的时段不解锁新额度
// （累计目标与上一时段持平）；曲线开头的零权重时段累计目标为 0。
// 最后一个时段的前缀恰为总权重，因此其累计目标恒等于日预算。
// Curve 是不可变值。
type Curve struct {
	weights []int64
	prefix  []*big.Int // prefix[h] = weights[0..h] 之和
	total   *big.Int
}

// NewCurve 校验并构造投放曲线。
func NewCurve(weights []int64) (*Curve, error) {
	if len(weights) != SlotsPerDay {
		return nil, fmt.Errorf("domain: pacing curve must have exactly %d hourly weights, got %d",
			SlotsPerDay, len(weights))
	}
	cp := make([]int64, SlotsPerDay)
	prefix := make([]*big.Int, SlotsPerDay)
	total := new(big.Int)
	for i, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("domain: pacing curve weight at slot %d is negative: %d", i, w)
		}
		cp[i] = w
		total.Add(total, big.NewInt(w))
		prefix[i] = new(big.Int).Set(total)
	}
	if total.Sign() == 0 {
		return nil, fmt.Errorf("domain: pacing curve weights must sum to a positive value")
	}
	return &Curve{weights: cp, prefix: prefix, total: total}, nil
}

// Weights 返回各时段权重的拷贝。
func (c *Curve) Weights() []int64 {
	out := make([]int64, len(c.weights))
	copy(out, c.weights)
	return out
}

// TotalWeight 返回权重总和。
func (c *Curve) TotalWeight() *big.Int { return new(big.Int).Set(c.total) }

// CumulativeTarget 返回日预算 cap 下到 slot 结束时的累计目标（向上取整，
// 避免整除截断让最后一个时段达不到日预算）。
func (c *Curve) CumulativeTarget(cap money.Money, slot int) money.Money {
	if slot < 0 {
		slot = 0
	} else if slot >= SlotsPerDay {
		slot = SlotsPerDay - 1
	}
	num := new(big.Int).Mul(cap.MinorBig(), c.prefix[slot])
	q, r := new(big.Int).QuoRem(num, c.total, new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return money.FromMinorBig(q, cap.Currency())
}

// equalWeights 判断曲线权重是否与 weights 完全一致。
func (c *Curve) equalWeights(weights []int64) bool {
	if c == nil || len(weights) != len(c.weights) {
		return false
	}
	for i := range c.weights {
		if c.weights[i] != weights[i] {
			return false
		}
	}
	return true
}

// slotOf 按活动时区返回 t 所在的小时时段（0..23）。
// t 必须来自统一时钟来源；本函数只做时区换算，不自行取时。
func slotOf(t time.Time, loc *time.Location) int {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Hour()
}
