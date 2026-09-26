package adinventory

import "time"

// ReservationStatus 是凭证的生命周期状态。
type ReservationStatus string

const (
	// StatusReserved 已预占，金额被持有，等待核销。
	StatusReserved ReservationStatus = "reserved"
	// StatusCaptured 已按实际费用核销（终态）。
	StatusCaptured ReservationStatus = "captured"
	// StatusCancelled 主动取消（终态）。
	StatusCancelled ReservationStatus = "cancelled"
	// StatusExpired 超时未核销，系统过期释放（终态）。
	StatusExpired ReservationStatus = "expired"
)

// IsFinal 报告状态是否为终态。
func (s ReservationStatus) IsFinal() bool {
	return s == StatusCaptured || s == StatusCancelled || s == StatusExpired
}

// Campaign 是广告活动配置：同时具有总预算与每自然日预算上限。
type Campaign struct {
	ID          string    `json:"id"`
	TotalBudget Money     `json:"total_budget"`
	DailyBudget Money     `json:"daily_budget"`
	CreatedAt   time.Time `json:"created_at"`
}

// Reservation 是一次成功预占返回的凭证。
type Reservation struct {
	Token       string            `json:"token"`
	CampaignID  string            `json:"campaign_id"`
	RequestNo   string            `json:"request_no"`
	Day         string            `json:"day"` // 预占发生的自然日（YYYY-MM-DD）
	Amount      Money             `json:"amount"`
	Payload     string            `json:"payload,omitempty"` // 请求内容指纹的一部分
	Status      ReservationStatus `json:"status"`
	ReceiptNo   string            `json:"receipt_no,omitempty"`
	ActualCost  Money             `json:"actual_cost"`
	CreatedAt   time.Time         `json:"created_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	FinalizedAt *time.Time        `json:"finalized_at,omitempty"`
}

// Balance 是活动在指定自然日的额度视图。
// 恒等式：Reserved + Captured <= Budget，Available = Budget - Reserved - Captured。
type Balance struct {
	CampaignID string `json:"campaign_id"`
	Day        string `json:"day"`

	TotalBudget    Money `json:"total_budget"`
	TotalReserved  Money `json:"total_reserved"`
	TotalCaptured  Money `json:"total_captured"`
	TotalAvailable Money `json:"total_available"`

	DailyBudget  Money `json:"daily_budget"`
	DayReserved  Money `json:"day_reserved"`
	DayCaptured  Money `json:"day_captured"`
	DayAvailable Money `json:"day_available"`
}
