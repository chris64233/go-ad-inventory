package domain

import (
	"encoding/json"
	"time"
)

// 事件类型常量。
const (
	EvCampaignCreated = "campaign.created"
	EvBudgetAdjusted  = "campaign.budget_adjusted"
	EvConfigAdjusted  = "campaign.config_adjusted"
	EvCampaignPaused  = "campaign.paused"
	EvCampaignResumed = "campaign.resumed"
	EvReserved        = "reservation.reserved"
	EvCaptured        = "reservation.captured"
	EvCancelled       = "reservation.cancelled"
	EvExpired         = "reservation.expired"
)

// evtMoney 是事件中的金额线格式：最小单位整数序列化为字符串以保证精度。
type evtMoney struct {
	Minor    string `json:"minor"`
	Currency string `json:"currency"`
}

// CampaignCreatedData 是 EvCampaignCreated 的负载。
// Curve 为每日 24 时段投放曲线权重；空表示均匀曲线。
type CampaignCreatedData struct {
	CampaignID  string        `json:"campaign_id"`
	Name        string        `json:"name"`
	TotalBudget evtMoney      `json:"total_budget"`
	DailyCap    evtMoney      `json:"daily_cap"`
	Timezone    string        `json:"timezone"`
	DefaultTTL  time.Duration `json:"default_ttl"`
	Curve       []int64       `json:"curve,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
}

// BudgetAdjustedData 是 EvBudgetAdjusted 的负载：带配置版本的预算调整。
// ExpectedVersion 为乐观锁版本；TotalBudget/DailyCap 为调整后的完整新上限。
type BudgetAdjustedData struct {
	CampaignID      string    `json:"campaign_id"`
	AdjustmentID    string    `json:"adjustment_id"`
	ExpectedVersion int64     `json:"expected_version"`
	NewVersion      int64     `json:"new_version"`
	TotalBudget     evtMoney  `json:"total_budget"`
	DailyCap        evtMoney  `json:"daily_cap"`
	Reason          string    `json:"reason,omitempty"`
	At              time.Time `json:"at"`
}

// ConfigAdjustedData 是 EvConfigAdjusted 的负载：切换时区和/或投放曲线。
// HasTimezone/HasCurve 区分“未提供（不变）”与“提供了零值”；
// RemoveCurve=true 表示显式移除节奏曲线（此时 Curve 必为 nil）。
type ConfigAdjustedData struct {
	CampaignID      string    `json:"campaign_id"`
	AdjustmentID    string    `json:"adjustment_id"`
	ExpectedVersion int64     `json:"expected_version"`
	NewVersion      int64     `json:"new_version"`
	Timezone        string    `json:"timezone,omitempty"`
	HasTimezone     bool      `json:"has_timezone"`
	Curve           []int64   `json:"curve,omitempty"`
	HasCurve        bool      `json:"has_curve"`
	RemoveCurve     bool      `json:"remove_curve,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	At              time.Time `json:"at"`
}

// StatusChangedData 是 EvCampaignPaused / EvCampaignResumed 的负载。
// Reason 记录本次状态变化的原因与依据，随事件永久保存。
type StatusChangedData struct {
	CampaignID string    `json:"campaign_id"`
	Reason     string    `json:"reason,omitempty"`
	At         time.Time `json:"at"`
}

// ReservedData 是 EvReserved 的负载。一次预占同时占用总预算、day_key 日预算
// 与 day_key 当天 slot 时段的节奏额度；冻结的 Version 保证事后配置切换
// 不改变该笔费用的归属。
type ReservedData struct {
	ReservationID string    `json:"reservation_id"`
	CampaignID    string    `json:"campaign_id"`
	RequestID     string    `json:"request_id"`
	Amount        evtMoney  `json:"amount"`
	DayKey        string    `json:"day_key"`
	Slot          int       `json:"slot"`
	Version       int64     `json:"config_version"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// CapturedData 是 EvCaptured 的负载：captured 为实际核销额，
// released = 预占额 - 核销额 被释放回总预算/日预算/时段节奏额度。
// 费用归入预占冻结的 DayKey/Slot，绝不因迟到回执而被重新归类。
type CapturedData struct {
	ReservationID string    `json:"reservation_id"`
	CampaignID    string    `json:"campaign_id"`
	ReceiptID     string    `json:"receipt_id"`
	Captured      evtMoney  `json:"captured"`
	Released      evtMoney  `json:"released"`
	DayKey        string    `json:"day_key"`
	Slot          int       `json:"slot"`
	At            time.Time `json:"at"`
}

// CancelledData 是 EvCancelled 的负载，释放全部预占额。
type CancelledData struct {
	ReservationID string    `json:"reservation_id"`
	CampaignID    string    `json:"campaign_id"`
	Released      evtMoney  `json:"released"`
	DayKey        string    `json:"day_key"`
	Slot          int       `json:"slot"`
	At            time.Time `json:"at"`
}

// ExpiredData 是 EvExpired 的负载，释放全部预占额。
type ExpiredData struct {
	ReservationID string    `json:"reservation_id"`
	CampaignID    string    `json:"campaign_id"`
	Released      evtMoney  `json:"released"`
	DayKey        string    `json:"day_key"`
	Slot          int       `json:"slot"`
	At            time.Time `json:"at"`
}

// marshalEvent 把类型化负载打包为信封可直接 Append 的 store.Event。
func marshalEvent(typ string, at time.Time, v any) (e rawEvent, err error) {
	data, err := json.Marshal(v)
	if err != nil {
		return rawEvent{}, err
	}
	return rawEvent{Type: typ, At: at, Data: data}, nil
}

// rawEvent 与 store.Event 同构，domain 不直接依赖 store 以避免循环。
type rawEvent struct {
	Type string
	At   time.Time
	Data json.RawMessage
}
