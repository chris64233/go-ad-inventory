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

// Campaign 是活动配置的投影。
type Campaign struct {
	ID          string
	Name        string
	TotalBudget money.Money
	DailyCap    money.Money
	Location    *time.Location
	DefaultTTL  time.Duration
	CreatedAt   time.Time
}

// Reservation 是一张预占凭证的投影。
type Reservation struct {
	ID         string
	CampaignID string
	RequestID  string
	Amount     money.Money
	DayKey     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Status     ReservationStatus
	Captured   money.Money
	ReceiptID  string
	TerminalAt time.Time
}

// dayBalance 记录单个自然日的已核销额与预占占用额。
type dayBalance struct {
	spent    money.Money
	reserved money.Money
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
		}
		cs.days[key] = d
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

func (cs *campaignState) applyReserved(amount money.Money, dayKey string) {
	cs.totalReserved = cs.totalReserved.Add(amount)
	d := cs.day(dayKey)
	d.reserved = d.reserved.Add(amount)
}

func (cs *campaignState) applyCaptured(amount, captured money.Money, dayKey string) {
	cs.totalReserved = cs.totalReserved.Sub(amount)
	cs.totalSpent = cs.totalSpent.Add(captured)
	d := cs.day(dayKey)
	d.reserved = d.reserved.Sub(amount)
	d.spent = d.spent.Add(captured)
}

func (cs *campaignState) applyReleased(amount money.Money, dayKey string) {
	cs.totalReserved = cs.totalReserved.Sub(amount)
	d := cs.day(dayKey)
	d.reserved = d.reserved.Sub(amount)
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
