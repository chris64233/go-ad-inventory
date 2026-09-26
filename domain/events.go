package domain

import (
	"encoding/json"
	"time"
)

// 事件类型常量。
const (
	EvCampaignCreated = "campaign.created"
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
type CampaignCreatedData struct {
	CampaignID  string        `json:"campaign_id"`
	Name        string        `json:"name"`
	TotalBudget evtMoney      `json:"total_budget"`
	DailyCap    evtMoney      `json:"daily_cap"`
	Timezone    string        `json:"timezone"`
	DefaultTTL  time.Duration `json:"default_ttl"`
	CreatedAt   time.Time     `json:"created_at"`
}

// ReservedData 是 EvReserved 的负载。一次预占同时占用总预算与 day_key 日预算。
type ReservedData struct {
	ReservationID string    `json:"reservation_id"`
	CampaignID    string    `json:"campaign_id"`
	RequestID     string    `json:"request_id"`
	Amount        evtMoney  `json:"amount"`
	DayKey        string    `json:"day_key"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// CapturedData 是 EvCaptured 的负载：captured 为实际核销额，
// released = 预占额 - 核销额 被释放回两级预算。
type CapturedData struct {
	ReservationID string    `json:"reservation_id"`
	CampaignID    string    `json:"campaign_id"`
	ReceiptID     string    `json:"receipt_id"`
	Captured      evtMoney  `json:"captured"`
	Released      evtMoney  `json:"released"`
	DayKey        string    `json:"day_key"`
	At            time.Time `json:"at"`
}

// CancelledData 是 EvCancelled 的负载，释放全部预占额。
type CancelledData struct {
	ReservationID string    `json:"reservation_id"`
	CampaignID    string    `json:"campaign_id"`
	Released      evtMoney  `json:"released"`
	At            time.Time `json:"at"`
}

// ExpiredData 是 EvExpired 的负载，释放全部预占额。
type ExpiredData struct {
	ReservationID string    `json:"reservation_id"`
	CampaignID    string    `json:"campaign_id"`
	Released      evtMoney  `json:"released"`
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
