package domain

import (
	"fmt"
	"math/big"
	"time"

	"github.com/chris64233/go-ad-inventory/money"
)

// InitialConfigVersion 是活动创建时的配置版本；每次配置切换或预算调整递增 1。
const InitialConfigVersion int64 = 1

// errBadEvent 表示持久化事件损坏（不应在正常运行中出现）。
func errBadEvent(format string, args ...any) error {
	return fmt.Errorf("domain: corrupt event: "+format, args...)
}

// ReservationStatus 是凭证的生命周期状态。
type ReservationStatus string

const (
	// StatusReserved 已预占、等待回执。
	StatusReserved ReservationStatus = "reserved"
	// StatusCaptured 已核销（终态）。
	StatusCaptured ReservationStatus = "captured"
	// StatusCancelled 主动取消（终态）。
	StatusCancelled ReservationStatus = "cancelled"
	// StatusExpired 超时过期（终态）。
	StatusExpired ReservationStatus = "expired"
)

// IsTerminal 判断是否终态。
func (s ReservationStatus) IsTerminal() bool {
	return s == StatusCaptured || s == StatusCancelled || s == StatusExpired
}

// Campaign 是活动配置的投影。
type Campaign struct {
	ID            string
	Name          string
	TotalBudget   money.Money
	DailyCap      money.Money
	Location      *time.Location
	DefaultTTL    time.Duration
	ConfigVersion int64
	// Curve 为 nil 表示该活动不做时段节奏限制。
	Curve     *PacingCurve
	CreatedAt time.Time
}

// Reservation 是一张预占凭证的投影。
//
// DayKey/Slot/ConfigVersion 在创建时刻按当时的活动时区与配置版本冻结，
// 之后核销、取消、过期的所有额度变动都只回到这个冻结桶：
// 迟到回执不会被新时区或新曲线重新归类。
type Reservation struct {
	ID            string
	CampaignID    string
	RequestID     string
	Amount        money.Money
	DayKey        string
	Slot          int
	ConfigVersion int64
	CreatedAt     time.Time
	ExpiresAt     time.Time
	Status        ReservationStatus
	Captured      money.Money
	ReceiptID     string
	TerminalAt    time.Time
}

// slotBucket 记录单个小时桶内的已核销额与预占占用额。
type slotBucket struct {
	spent    money.Money
	reserved money.Money
}

// dayBalance 记录单个自然日（以及其中 24 个小时桶）的核销与预占。
type dayBalance struct {
	spent    money.Money
	reserved money.Money
	slots    [SlotCount]slotBucket
}

// campaignState 是单个活动的完整聚合状态。
type campaignState struct {
	campaign *Campaign
	// 总预算维度。
	totalSpent    money.Money
	totalReserved money.Money
	// 日预算维度，key 为 "2006-01-02"。
	days map[string]*dayBalance
	// 预算调整历史，按应用顺序排列。
	adjustments []BudgetAdjustment
}

func newCampaignState(c *Campaign) *campaignState {
	zero := money.Zero(c.TotalBudget.Currency())
	return &campaignState{
		campaign:      c,
		totalSpent:    zero,
		totalReserved: zero,
		days:          make(map[string]*dayBalance),
	}
}

func (cs *campaignState) day(key string) *dayBalance {
	d, ok := cs.days[key]
	if !ok {
		d = newDayBalance(cs.campaign.TotalBudget.Currency())
		cs.days[key] = d
	}
	return d
}

func newDayBalance(currency string) *dayBalance {
	d := &dayBalance{}
	zero := money.Zero(currency)
	d.spent = zero
	d.reserved = zero
	for h := range d.slots {
		d.slots[h] = slotBucket{spent: zero, reserved: zero}
	}
	return d
}

// totalAvailable = 总预算 - 已核销 - 未完成预占。
func (cs *campaignState) totalAvailable() money.Money {
	return cs.campaign.TotalBudget.Sub(cs.totalSpent).Sub(cs.totalReserved)
}

// dailyAvailable = 日上限 - 当日已核销 - 当日未完成预占。
func (cs *campaignState) dailyAvailable(key string) money.Money {
	d := cs.day(key)
	return cs.campaign.DailyCap.Sub(d.spent).Sub(d.reserved)
}

// pacingAvailable 返回 dayKey 当天截至 slot（含）允许的累计节奏消耗额度。
// 未配置曲线时节奏额度等于日上限，即第三级检查退化为日预算检查。
//
// 注意：计算只按当前时刻的曲线；历史桶的归属在预占时已经冻结，
// 曲线切换只影响切换之后新创建的预占。
func (cs *campaignState) pacingAvailable(dayKey string, slot int) money.Money {
	cap := cs.campaign.DailyCap
	if cs.campaign.Curve != nil {
		cap = scaledFloor(cs.campaign.DailyCap, cs.campaign.Curve.CumulativeTargetPPM(slot))
	}
	d := cs.day(dayKey)
	return cap.Sub(d.spent).Sub(d.reserved)
}

// pacingCap 返回截至 slot 的累计节奏目标金额（不含已用额）。
func (cs *campaignState) pacingCap(slot int) money.Money {
	if cs.campaign.Curve == nil {
		return cs.campaign.DailyCap
	}
	return scaledFloor(cs.campaign.DailyCap, cs.campaign.Curve.CumulativeTargetPPM(slot))
}

func (cs *campaignState) applyReserved(amount money.Money, dayKey string, slot int) {
	cs.totalReserved = cs.totalReserved.Add(amount)
	d := cs.day(dayKey)
	d.reserved = d.reserved.Add(amount)
	if slot >= 0 && slot < SlotCount {
		b := &d.slots[slot]
		b.reserved = b.reserved.Add(amount)
	}
}

func (cs *campaignState) applyCaptured(amount, captured money.Money, dayKey string, slot int) {
	cs.totalReserved = cs.totalReserved.Sub(amount)
	cs.totalSpent = cs.totalSpent.Add(captured)
	d := cs.day(dayKey)
	d.reserved = d.reserved.Sub(amount)
	d.spent = d.spent.Add(captured)
	if slot >= 0 && slot < SlotCount {
		b := &d.slots[slot]
		b.reserved = b.reserved.Sub(amount)
		b.spent = b.spent.Add(captured)
	}
}

func (cs *campaignState) applyReleased(amount money.Money, dayKey string, slot int) {
	cs.totalReserved = cs.totalReserved.Sub(amount)
	d := cs.day(dayKey)
	d.reserved = d.reserved.Sub(amount)
	if slot >= 0 && slot < SlotCount {
		b := &d.slots[slot]
		b.reserved = b.reserved.Sub(amount)
	}
}

// 金额与事件格式互转。

func toEvt(m money.Money) evtMoney {
	// Money 不暴露内部 big.Int，这里通过字符串往返；增加一个 Minor 访问器更直接。
	return evtMoney{Minor: m.MinorString(), Currency: m.Currency()}
}

func fromEvt(e evtMoney) (money.Money, error) {
	v, ok := new(big.Int).SetString(e.Minor, 10)
	if !ok {
		return money.Money{}, errBadEvent("invalid minor units %q", e.Minor)
	}
	return money.FromMinorBig(v, e.Currency), nil
}
