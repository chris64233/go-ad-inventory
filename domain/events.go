package domain

import (
	"encoding/json"
	"time"
)

// 事件类型常量。
const (
	EvCampaignCreated = "campaign.created"
	// EvConfigUpdated 变更活动时区与/或投放曲线（配置版本递增）。
	EvConfigUpdated = "campaign.config_updated"
	// EvBudgetAdjusted 预算调整（提高或降低），每次调整版本递增。
	EvBudgetAdjusted = "campaign.budget_adjusted"
	EvReserved       = "reservation.reserved"
	EvCaptured       = "reservation.captured"
	EvCancelled      = "reservation.cancelled"
	EvExpired        = "reservation.expired"
)

// evtMoney 是事件中的金额线格式：最小单位整数序列化为字符串以保证精度。
type evtMoney struct {
	Minor    string `json:"minor"`
	Currency string `json:"currency"`
}

// CampaignCreatedData 是 EvCampaignCreated 的负载。
type CampaignCreatedData struct {
	CampaignID    string        `json:"campaign_id"`
	Name          string        `json:"name"`
	TotalBudget   evtMoney      `json:"total_budget"`
	DailyCap      evtMoney      `json:"daily_cap"`
	Timezone      string        `json:"timezone"`
	DefaultTTL    time.Duration `json:"default_ttl"`
	ConfigVersion int64         `json:"config_version"`
	// Curve 为 nil 表示该活动不做时段节奏限制（null 落盘）。
	Curve     *PacingCurve `json:"curve,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

// ConfigUpdatedData 是 EvConfigUpdated 的负载：切换时区与/或投放曲线。
// 历史预占凭证在创建时已冻结日期、时段与配置版本，不受本次切换影响。
type ConfigUpdatedData struct {
	CampaignID    string `json:"campaign_id"`
	ConfigVersion int64  `json:"config_version"`
	Timezone      string `json:"timezone"`
	// Curve 显式序列化为 null 表示移除曲线；否则整体替换为新曲线。
	Curve *PacingCurve `json:"curve"`
	At    time.Time    `json:"at"`
}

// BudgetAdjustedData 是 EvBudgetAdjusted 的负载，记录调整后的完整预算快照。
type BudgetAdjustedData struct {
	CampaignID    string    `json:"campaign_id"`
	AdjustmentID  string    `json:"adjustment_id"`
	ConfigVersion int64     `json:"config_version"`
	TotalBudget   evtMoney  `json:"total_budget"`
	DailyCap      evtMoney  `json:"daily_cap"`
	At            time.Time `json:"at"`
}

// ReservedData 是 EvReserved 的负载。一次预占同时占用总预算、day_key 日预算
// 以及（配置曲线时）slot 之前的累计节奏额度。DayKey/Slot/ConfigVersion
// 在创建时刻冻结，之后任何核销与释放都只回到这个冻结桶。
type ReservedData struct {
	ReservationID string    `json:"reservation_id"`
	CampaignID    string    `json:"campaign_id"`
	RequestID     string    `json:"request_id"`
	Amount        evtMoney  `json:"amount"`
	DayKey        string    `json:"day_key"`
	Slot          int       `json:"slot"`
	ConfigVersion int64     `json:"config_version"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// CapturedData 是 EvCaptured 的负载：captured 为实际核销额，
// released = 预占额 - 核销额 被释放回两级预算与冻结时段桶。
type CapturedData struct {
	ReservationID string    `json:"reservation_id"`
	CampaignID    string    `json:"campaign_id"`
	ReceiptID     string    `json:"receipt_id"`
	Captured      evtMoney  `json:"captured"`
	Released      evtMoney  `json:"released"`
	DayKey        string    `json:"day_key"`
	Slot          int       `json:"slot"`
	ConfigVersion int64     `json:"config_version"`
	At            time.Time `json:"at"`
}

// releaseData 是 EvCancelled / EvExpired 的公共负载。
// 释放坐标（DayKey/Slot）来自凭证创建时的冻结值并冗余进事件，
// 重放时即使配置已切换也能精确归还原桶。
type releaseData struct {
	ReservationID string    `json:"reservation_id"`
	CampaignID    string    `json:"campaign_id"`
	Released      evtMoney  `json:"released"`
	DayKey        string    `json:"day_key"`
	Slot          int       `json:"slot"`
	ConfigVersion int64     `json:"config_version"`
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
