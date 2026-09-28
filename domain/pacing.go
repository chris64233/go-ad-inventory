package domain

import (
	"fmt"
	"math/big"

	"github.com/chris64233/go-ad-inventory/money"
)

// PacingPPM 是投放曲线的比例基数：百万分之一（parts per million）。
// 曲线点全部以 0..PacingPPM 的整数表示，全程定点运算、不引入浮点误差。
const PacingPPM int64 = 1_000_000

// SlotCount 是每日投放时段数：按活动时区切分的 24 个整点小时桶。
const SlotCount = 24

// PacingCurve 是每日投放曲线：Targets[h] 表示到第 h 个小时桶结束时
// （即本地时间 h+1 点），当日允许累计消耗占日上限的比例（ppm）。
//
// 约束（见 Validate）：
//   - 长度必须为 SlotCount（24）；
//   - 取值范围 [0, PacingPPM]，单调非减；
//   - 最后一个点必须为 PacingPPM（日末允许投满日上限）。
//
// 例如 Targets[9] = 500000 表示上午结束前最多投掉日上限的一半。
type PacingCurve struct {
	Targets []int64 `json:"targets_ppm"`
}

// EvenPacingCurve 返回均匀投放曲线：每小时线性增长，末日点强制为 100%。
func EvenPacingCurve() PacingCurve {
	targets := make([]int64, SlotCount)
	for h := 0; h < SlotCount; h++ {
		// 整数除法天然向下取整；末点单独置满，保证单调且收满。
		targets[h] = int64(h+1) * PacingPPM / SlotCount
	}
	targets[SlotCount-1] = PacingPPM
	return PacingCurve{Targets: targets}
}

// Validate 校验曲线形状合法。
func (c PacingCurve) Validate() error {
	if len(c.Targets) != SlotCount {
		return fmt.Errorf("pacing curve must have exactly %d hourly targets, got %d",
			SlotCount, len(c.Targets))
	}
	prev := int64(0)
	for h, v := range c.Targets {
		if v < 0 || v > PacingPPM {
			return fmt.Errorf("pacing curve target at hour %d = %d, out of range [0,%d]",
				h, v, PacingPPM)
		}
		if v < prev {
			return fmt.Errorf("pacing curve must be non-decreasing: hour %d target %d < previous %d",
				h, v, prev)
		}
		prev = v
	}
	if prev != PacingPPM {
		return fmt.Errorf("pacing curve last target must be %d (100%%), got %d",
			PacingPPM, prev)
	}
	return nil
}

// CumulativeTargetPPM 返回到第 slot 个小时桶的累计目标比例。
// slot 越界（理论上不会发生）按就近端点钳制。
func (c PacingCurve) CumulativeTargetPPM(slot int) int64 {
	if slot <= 0 {
		return c.Targets[0]
	}
	if slot >= SlotCount {
		return c.Targets[SlotCount-1]
	}
	return c.Targets[slot]
}

// scaledFloor 返回 floor(m * weight / PacingPPM)。
// 节奏目标金额始终向下取整，保证节奏额度永远不会被取整放大。
func scaledFloor(m money.Money, weight int64) money.Money {
	v, _ := new(big.Int).SetString(m.MinorString(), 10)
	v.Mul(v, big.NewInt(weight))
	v.Quo(v, big.NewInt(PacingPPM))
	return money.FromMinorBig(v, m.Currency())
}

// SlotPacing 是单个小时桶的节奏视图。
type SlotPacing struct {
	// Hour 为小时桶序号（活动本地时区 0..23）。
	Hour int `json:"hour"`
	// CumulativeTargetPPM 为到该小时桶结束的累计目标比例（ppm）。
	CumulativeTargetPPM int64 `json:"cumulative_target_ppm"`
	// Target 为累计目标金额（日上限 × 累计比例）。
	Target money.Money `json:"target"`
	// Spent 为创建时冻结到本桶的实际核销额。
	Spent money.Money `json:"spent"`
	// Reserved 为创建时冻结到本桶的有效预占额。
	Reserved money.Money `json:"reserved"`
	// CumulativeSpent 为从首桶到本桶的累计核销。
	CumulativeSpent money.Money `json:"cumulative_spent"`
	// Variance 为累计核销相对目标的偏差：正=超出目标，负=欠投。
	Variance money.Money `json:"variance"`
}

// PacingReport 是某一自然日的逐时段节奏报告。
type PacingReport struct {
	CampaignID    string      `json:"campaign_id"`
	ConfigVersion int64       `json:"config_version"`
	Timezone      string      `json:"timezone"`
	DayKey        string      `json:"day_key"`
	DailyCap      money.Money `json:"daily_cap"`
	// CurveEnabled 报告生成时活动是否配置了投放曲线；
	// 未配置时每个桶的目标都等于日上限（即不做节奏限制）。
	CurveEnabled bool `json:"curve_enabled"`
	// Slots 按小时桶顺序排列的 24 行。
	Slots []SlotPacing `json:"slots"`
	// TotalSpent / TotalReserved 为该日跨全部桶的核销与有效预占汇总。
	TotalSpent    money.Money `json:"total_spent"`
	TotalReserved money.Money `json:"total_reserved"`
}
