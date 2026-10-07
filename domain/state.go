package domain

import (
	"fmt"
	"math/big"
	"time"

	"github.com/chris64233/go-ad-inventory/money"
)

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

// CampaignStatus 是活动的投放状态。
type CampaignStatus string

const (
	// CampaignActive 正常投放：预占/核销按预算与节奏计划执行。
	CampaignActive CampaignStatus = "active"
	// CampaignPaused 暂停投放：拒绝新的预占（不产生任何消耗记录），
	// 已确认的回执核销仍入账，恢复后从已确认消耗继续。
	CampaignPaused CampaignStatus = "paused"
)

// Campaign 是活动配置的投影。Version 从 1 起，每次预算调整加 1。
type Campaign struct {
	ID          string
	Name        string
	Status      CampaignStatus
	TotalBudget money.Money
	DailyCap    money.Money
	Location    *time.Location
	DefaultTTL  time.Duration
	Curve       *Curve
	Version     int64
	CreatedAt   time.Time
}

// Reservation 是一张预占凭证的投影。
//
// DayKey/Slot/Version 在预占创建时冻结：此后即使活动时区、投放曲线或预算
// 被调整，该笔费用（核销/释放）仍归入冻结的日期与时段，绝不重新归类。
type Reservation struct {
	ID         string
	CampaignID string
	RequestID  string
	Amount     money.Money
	DayKey     string
	Slot       int
	Version    int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Status     ReservationStatus
	Captured   money.Money
	ReceiptID  string
	TerminalAt time.Time
}

// slotBalance 记录单个时段的已核销额与预占占用额。
type slotBalance struct {
	spent    money.Money
	reserved money.Money
}

// dayBalance 记录单个自然日的已核销额、预占占用额以及逐时段明细。
type dayBalance struct {
	spent    money.Money
	reserved money.Money
	// key 为小时时段 0..23，惰性创建。
	slots map[int]*slotBalance
}

// campaignState 是单个活动的完整聚合状态。
type campaignState struct {
	campaign *Campaign
	// 总预算维度。
	totalSpent    money.Money
	totalReserved money.Money
	// 日预算维度，key 为 "2006-01-02"。
	days map[string]*dayBalance
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
		d = &dayBalance{
			spent:    money.Zero(cs.campaign.TotalBudget.Currency()),
			reserved: money.Zero(cs.campaign.TotalBudget.Currency()),
			slots:    make(map[int]*slotBalance),
		}
		cs.days[key] = d
	}
	return d
}

func (d *dayBalance) slot(h int) *slotBalance {
	s, ok := d.slots[h]
	if !ok {
		s = &slotBalance{
			spent:    money.Zero(d.spent.Currency()),
			reserved: money.Zero(d.spent.Currency()),
		}
		d.slots[h] = s
	}
	return s
}

// totalAvailable = 总预算 - 已核销 - 未完成预占。
func (cs *campaignState) totalAvailable() money.Money {
	return cs.campaign.TotalBudget.Sub(cs.totalSpent).Sub(cs.totalReserved)
}

// totalCommitted = 已核销 + 有效预占，是总预算不得低于的金额。
func (cs *campaignState) totalCommitted() money.Money {
	return cs.totalSpent.Add(cs.totalReserved)
}

// dailyAvailable = 日上限 - 当日已核销 - 当日未完成预占。
func (cs *campaignState) dailyAvailable(key string) money.Money {
	d := cs.day(key)
	return cs.campaign.DailyCap.Sub(d.spent).Sub(d.reserved)
}

// dailyCommitted = 当日已核销 + 当日有效预占。
func (cs *campaignState) dailyCommitted(key string) money.Money {
	d := cs.day(key)
	return d.spent.Add(d.reserved)
}

// slotTarget 返回当前配置下到 slot 结束时的累计节奏目标。
func (cs *campaignState) slotTarget(slot int) money.Money {
	return cs.campaign.Curve.CumulativeTarget(cs.campaign.DailyCap, slot)
}

// slotAvailable = 时段累计目标 - 当天已核销 - 当天未完成预占。
// 已核销与有效预占都计入“当前累计消耗”，因此未核销的预占本身也受节奏限制。
func (cs *campaignState) slotAvailable(key string, slot int) money.Money {
	d := cs.day(key)
	return cs.slotTarget(slot).Sub(d.spent).Sub(d.reserved)
}

func (cs *campaignState) applyReserved(amount money.Money, dayKey string, slot int) {
	cs.totalReserved = cs.totalReserved.Add(amount)
	d := cs.day(dayKey)
	d.reserved = d.reserved.Add(amount)
	s := d.slot(slot)
	s.reserved = s.reserved.Add(amount)
}

func (cs *campaignState) applyCaptured(amount, captured money.Money, dayKey string, slot int) {
	cs.totalReserved = cs.totalReserved.Sub(amount)
	cs.totalSpent = cs.totalSpent.Add(captured)
	d := cs.day(dayKey)
	d.reserved = d.reserved.Sub(amount)
	d.spent = d.spent.Add(captured)
	s := d.slot(slot)
	s.reserved = s.reserved.Sub(amount)
	s.spent = s.spent.Add(captured)
}

func (cs *campaignState) applyReleased(amount money.Money, dayKey string, slot int) {
	cs.totalReserved = cs.totalReserved.Sub(amount)
	d := cs.day(dayKey)
	d.reserved = d.reserved.Sub(amount)
	s := d.slot(slot)
	s.reserved = s.reserved.Sub(amount)
}

// applyBudget 仅替换配置上限（版本推进由事件负载携带，在 apply 时设置）。
func (cs *campaignState) applyBudget(total, daily money.Money) {
	cs.campaign.TotalBudget = total
	cs.campaign.DailyCap = daily
}

// 金额与事件格式互转。

func toEvt(m money.Money) evtMoney {
	return evtMoney{Minor: m.MinorString(), Currency: m.Currency()}
}

func fromEvt(e evtMoney) (money.Money, error) {
	v, ok := new(big.Int).SetString(e.Minor, 10)
	if !ok {
		return money.Money{}, errBadEvent("invalid minor units %q", e.Minor)
	}
	return money.FromMinorBig(v, e.Currency), nil
}

// uniformCurve 返回 24 个权重均为 1 的均匀投放曲线。
func uniformCurve() *Curve {
	weights := make([]int64, SlotsPerDay)
	for i := range weights {
		weights[i] = 1
	}
	c, err := NewCurve(weights)
	if err != nil {
		panic(err) // 24 个正权重，不可能失败
	}
	return c
}
