// Package httpapi 把领域服务暴露为 JSON HTTP 接口。
//
// 路由（Go 1.22+ 模式路由）：
//
//	POST /v1/campaigns                       创建活动
//	GET  /v1/campaigns/{id}                  查询活动配置
//	GET  /v1/campaigns/{id}/balance          查询总预算与当日预算余额
//	POST /v1/campaigns/{id}/reservations     预占（幂等键 request_id）
//	GET  /v1/reservations/{id}               查询凭证状态
//	POST /v1/reservations/{id}/capture       回执核销（去重键 receipt_id）
//	POST /v1/reservations/{id}/cancel        主动取消
//	POST /v1/admin/expire                    触发一次过期扫描
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/chris64233/go-ad-inventory/domain"
	"github.com/chris64233/go-ad-inventory/money"
)

// Server 是 HTTP 适配层。
type Server struct {
	svc *domain.Service
	mux *http.ServeMux
}

// NewServer 构造 HTTP 服务。
func NewServer(svc *domain.Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /v1/campaigns", s.createCampaign)
	s.mux.HandleFunc("GET /v1/campaigns/{id}", s.getCampaign)
	s.mux.HandleFunc("GET /v1/campaigns/{id}/balance", s.getBalance)
	s.mux.HandleFunc("POST /v1/campaigns/{id}/reservations", s.reserve)
	s.mux.HandleFunc("GET /v1/reservations/{id}", s.getReservation)
	s.mux.HandleFunc("POST /v1/reservations/{id}/capture", s.capture)
	s.mux.HandleFunc("POST /v1/reservations/{id}/cancel", s.cancel)
	s.mux.HandleFunc("POST /v1/admin/expire", s.expire)
	return s
}

// Handler 返回根 http.Handler。
func (s *Server) Handler() http.Handler { return s.mux }

// ---------- 线格式 ----------

type moneyIn struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func (m moneyIn) parse() (money.Money, error) {
	return money.Parse(m.Amount, m.Currency)
}

type createCampaignReq struct {
	Name        string   `json:"name"`
	TotalBudget moneyIn  `json:"total_budget"`
	DailyCap    moneyIn  `json:"daily_cap"`
	Timezone    string   `json:"timezone"`
	DefaultTTL  duration `json:"default_ttl"`
}

// duration 支持 "300s"、"5m" 或秒数两种 JSON 表示。
type duration time.Duration

func (d *duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case string:
		parsed, err := time.ParseDuration(t)
		if err != nil {
			return err
		}
		*d = duration(parsed)
	case float64:
		*d = duration(time.Duration(t) * time.Second)
	default:
		return errors.New("duration must be a string like \"5m\" or seconds number")
	}
	return nil
}

func (d duration) std() time.Duration { return time.Duration(d) }

type campaignResp struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	TotalBudget money.Money `json:"total_budget"`
	DailyCap    money.Money `json:"daily_cap"`
	Timezone    string      `json:"timezone"`
	DefaultTTL  string      `json:"default_ttl"`
	CreatedAt   time.Time   `json:"created_at"`
}

func toCampaignResp(c *domain.Campaign) campaignResp {
	return campaignResp{
		ID:          c.ID,
		Name:        c.Name,
		TotalBudget: c.TotalBudget,
		DailyCap:    c.DailyCap,
		Timezone:    c.Location.String(),
		DefaultTTL:  c.DefaultTTL.String(),
		CreatedAt:   c.CreatedAt,
	}
}

type reserveReq struct {
	RequestID string   `json:"request_id"`
	Amount    moneyIn  `json:"amount"`
	TTL       duration `json:"ttl"`
}

type reservationResp struct {
	ID         string                   `json:"id"`
	CampaignID string                   `json:"campaign_id"`
	RequestID  string                   `json:"request_id"`
	Amount     money.Money              `json:"amount"`
	DayKey     string                   `json:"day_key"`
	Status     domain.ReservationStatus `json:"status"`
	Captured   money.Money              `json:"captured"`
	ReceiptID  string                   `json:"receipt_id,omitempty"`
	CreatedAt  time.Time                `json:"created_at"`
	ExpiresAt  time.Time                `json:"expires_at"`
}

func toReservationResp(r *domain.Reservation) reservationResp {
	return reservationResp{
		ID:         r.ID,
		CampaignID: r.CampaignID,
		RequestID:  r.RequestID,
		Amount:     r.Amount,
		DayKey:     r.DayKey,
		Status:     r.Status,
		Captured:   r.Captured,
		ReceiptID:  r.ReceiptID,
		CreatedAt:  r.CreatedAt,
		ExpiresAt:  r.ExpiresAt,
	}
}

type captureReq struct {
	ReceiptID string  `json:"receipt_id"`
	Amount    moneyIn `json:"amount"`
}

type balanceResp struct {
	CampaignID     string      `json:"campaign_id"`
	TotalBudget    money.Money `json:"total_budget"`
	TotalSpent     money.Money `json:"total_spent"`
	TotalReserved  money.Money `json:"total_reserved"`
	TotalAvailable money.Money `json:"total_available"`
	DayKey         string      `json:"day_key"`
	DailyCap       money.Money `json:"daily_cap"`
	DailySpent     money.Money `json:"daily_spent"`
	DailyReserved  money.Money `json:"daily_reserved"`
	DailyAvailable money.Money `json:"daily_available"`
}

type errorResp struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Level     string `json:"level,omitempty"`
	Requested string `json:"requested,omitempty"`
	Available string `json:"available,omitempty"`
}

// ---------- 处理器 ----------

func (s *Server) createCampaign(w http.ResponseWriter, r *http.Request) {
	var req createCampaignReq
	if !decode(w, r, &req) {
		return
	}
	total, err := req.TotalBudget.parse()
	if err != nil {
		writeDomainErr(w, invalid("total_budget: %v", err))
		return
	}
	daily, err := req.DailyCap.parse()
	if err != nil {
		writeDomainErr(w, invalid("daily_cap: %v", err))
		return
	}
	c, err := s.svc.CreateCampaign(r.Context(), domain.CreateCampaignParams{
		Name:        req.Name,
		TotalBudget: total,
		DailyCap:    daily,
		Timezone:    req.Timezone,
		DefaultTTL:  req.DefaultTTL.std(),
	})
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toCampaignResp(c))
}

func (s *Server) getCampaign(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.GetCampaign(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCampaignResp(c))
}

func (s *Server) getBalance(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.GetBalance(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, balanceResp{
		CampaignID:     b.CampaignID,
		TotalBudget:    b.TotalBudget,
		TotalSpent:     b.TotalSpent,
		TotalReserved:  b.TotalReserved,
		TotalAvailable: b.TotalAvailable,
		DayKey:         b.DayKey,
		DailyCap:       b.DailyCap,
		DailySpent:     b.DailySpent,
		DailyReserved:  b.DailyReserved,
		DailyAvailable: b.DailyAvailable,
	})
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	var req reserveReq
	if !decode(w, r, &req) {
		return
	}
	amount, err := req.Amount.parse()
	if err != nil {
		writeDomainErr(w, invalid("amount: %v", err))
		return
	}
	res, err := s.svc.Reserve(r.Context(), domain.ReserveParams{
		CampaignID: r.PathValue("id"),
		RequestID:  req.RequestID,
		Amount:     amount,
		TTL:        req.TTL.std(),
	})
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toReservationResp(res))
}

func (s *Server) getReservation(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.GetReservation(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toReservationResp(res))
}

func (s *Server) capture(w http.ResponseWriter, r *http.Request) {
	var req captureReq
	if !decode(w, r, &req) {
		return
	}
	amount, err := req.Amount.parse()
	if err != nil {
		writeDomainErr(w, invalid("amount: %v", err))
		return
	}
	res, err := s.svc.Capture(r.Context(), domain.CaptureParams{
		ReservationID: r.PathValue("id"),
		ReceiptID:     req.ReceiptID,
		Amount:        amount,
	})
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toReservationResp(res))
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toReservationResp(res))
}

func (s *Server) expire(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.ExpireSweep(r.Context())
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"expired": n})
}

// ---------- 辅助 ----------

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeDomainErr(w, invalid("invalid JSON body: %v", err))
		return false
	}
	return true
}

func invalid(format string, args ...any) error {
	return &domain.Error{Code: domain.CodeInvalidArgument, Message: fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeDomainErr 把领域错误映射为 HTTP 状态码与统一错误体。
func writeDomainErr(w http.ResponseWriter, err error) {
	de, ok := domain.AsError(err)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResp{Error: errorBody{
			Code:    "internal",
			Message: err.Error(),
		}})
		return
	}
	status := http.StatusInternalServerError
	switch de.Code {
	case domain.CodeInvalidArgument:
		status = http.StatusBadRequest
	case domain.CodeNotFound:
		status = http.StatusNotFound
	case domain.CodeBudgetExceeded:
		status = http.StatusUnprocessableEntity
	case domain.CodeConflict, domain.CodeIdempotencyConflict:
		status = http.StatusConflict
	}
	body := errorBody{Code: string(de.Code), Message: de.Message, Level: de.Level}
	if de.Code == domain.CodeBudgetExceeded {
		body.Requested = de.Requested.String()
		body.Available = de.Available.String()
	}
	writeJSON(w, status, errorResp{Error: body})
}
