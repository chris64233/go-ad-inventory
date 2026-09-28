// Package httpapi 把领域服务暴露为 JSON HTTP 接口。
//
// 路由（Go 1.22+ 模式路由）：
//
//	POST /v1/campaigns                          创建活动（时区、24 时段曲线）
//	GET  /v1/campaigns/{id}                     查询活动配置（含配置版本）
//	GET  /v1/campaigns/{id}/balance             查询总预算、当日预算与当前时段节奏
//	POST /v1/campaigns/{id}/reservations        预占（幂等键 request_id）
//	POST /v1/campaigns/{id}/budget-adjustments  带版本的预算调整（幂等键 adjustment_id）
//	POST /v1/campaigns/{id}/config-adjustments  切换时区/投放曲线（带版本）
//	GET  /v1/campaigns/{id}/versions            配置版本流
//	GET  /v1/campaigns/{id}/pacing              逐时段目标/核销/预占/偏差
//	GET  /v1/reservations/{id}                  查询凭证状态
//	POST /v1/reservations/{id}/capture          回执核销（去重键 receipt_id）
//	POST /v1/reservations/{id}/cancel           主动取消
//	POST /v1/admin/expire                       触发一次过期扫描
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
	s.mux.HandleFunc("POST /v1/campaigns/{id}/budget-adjustments", s.adjustBudget)
	s.mux.HandleFunc("POST /v1/campaigns/{id}/config-adjustments", s.adjustConfig)
	s.mux.HandleFunc("GET /v1/campaigns/{id}/versions", s.getVersions)
	s.mux.HandleFunc("GET /v1/campaigns/{id}/pacing", s.getPacing)
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
	Name         string   `json:"name"`
	TotalBudget  moneyIn  `json:"total_budget"`
	DailyCap     moneyIn  `json:"daily_cap"`
	Timezone     string   `json:"timezone"`
	DefaultTTL   duration `json:"default_ttl"`
	CurveWeights []int64  `json:"curve_weights"`
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
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	TotalBudget  money.Money `json:"total_budget"`
	DailyCap     money.Money `json:"daily_cap"`
	Timezone     string      `json:"timezone"`
	DefaultTTL   string      `json:"default_ttl"`
	CurveWeights []int64     `json:"curve_weights,omitempty"`
	Version      int64       `json:"config_version"`
	CreatedAt    time.Time   `json:"created_at"`
}

func toCampaignResp(c *domain.Campaign) campaignResp {
	resp := campaignResp{
		ID:          c.ID,
		Name:        c.Name,
		TotalBudget: c.TotalBudget,
		DailyCap:    c.DailyCap,
		Timezone:    c.Location.String(),
		DefaultTTL:  c.DefaultTTL.String(),
		Version:     c.Version,
		CreatedAt:   c.CreatedAt,
	}
	if c.Curve != nil {
		resp.CurveWeights = c.Curve.Weights()
	}
	return resp
}

type reserveReq struct {
	RequestID string   `json:"request_id"`
	Amount    moneyIn  `json:"amount"`
	TTL       duration `json:"ttl"`
}

type reservationResp struct {
	ID            string                   `json:"id"`
	CampaignID    string                   `json:"campaign_id"`
	RequestID     string                   `json:"request_id"`
	Amount        money.Money              `json:"amount"`
	DayKey        string                   `json:"day_key"`
	Slot          int                      `json:"slot"`
	ConfigVersion int64                    `json:"config_version"`
	Status        domain.ReservationStatus `json:"status"`
	Captured      money.Money              `json:"captured"`
	ReceiptID     string                   `json:"receipt_id,omitempty"`
	CreatedAt     time.Time                `json:"created_at"`
	ExpiresAt     time.Time                `json:"expires_at"`
}

func toReservationResp(r *domain.Reservation) reservationResp {
	return reservationResp{
		ID:            r.ID,
		CampaignID:    r.CampaignID,
		RequestID:     r.RequestID,
		Amount:        r.Amount,
		DayKey:        r.DayKey,
		Slot:          r.Slot,
		ConfigVersion: r.Version,
		Status:        r.Status,
		Captured:      r.Captured,
		ReceiptID:     r.ReceiptID,
		CreatedAt:     r.CreatedAt,
		ExpiresAt:     r.ExpiresAt,
	}
}

type captureReq struct {
	ReceiptID string  `json:"receipt_id"`
	Amount    moneyIn `json:"amount"`
}

type balanceResp struct {
	CampaignID     string      `json:"campaign_id"`
	ConfigVersion  int64       `json:"config_version"`
	TotalBudget    money.Money `json:"total_budget"`
	TotalSpent     money.Money `json:"total_spent"`
	TotalReserved  money.Money `json:"total_reserved"`
	TotalAvailable money.Money `json:"total_available"`
	DayKey         string      `json:"day_key"`
	Slot           int         `json:"slot"`
	PacingLimited  bool        `json:"pacing_limited"`
	DailyCap       money.Money `json:"daily_cap"`
	DailySpent     money.Money `json:"daily_spent"`
	DailyReserved  money.Money `json:"daily_reserved"`
	DailyAvailable money.Money `json:"daily_available"`
	SlotTarget     money.Money `json:"slot_target"`
	SlotAvailable  money.Money `json:"slot_available"`
}

type adjustBudgetReq struct {
	AdjustmentID    string  `json:"adjustment_id"`
	ExpectedVersion int64   `json:"expected_version"`
	TotalBudget     moneyIn `json:"total_budget"`
	DailyCap        moneyIn `json:"daily_cap"`
}

type versionResp struct {
	Version      int64       `json:"version"`
	Kind         string      `json:"kind"`
	AdjustmentID string      `json:"adjustment_id,omitempty"`
	TotalBudget  money.Money `json:"total_budget"`
	DailyCap     money.Money `json:"daily_cap"`
	Timezone     string      `json:"timezone"`
	CurveWeights []int64     `json:"curve_weights,omitempty"`
	At           time.Time   `json:"at"`
}

type slotPacingResp struct {
	Slot                int         `json:"slot"`
	Weight              int64       `json:"weight"`
	CumulativeTarget    money.Money `json:"cumulative_target"`
	SlotSpent           money.Money `json:"slot_spent"`
	SlotReserved        money.Money `json:"slot_reserved"`
	CumulativeSpent     money.Money `json:"cumulative_spent"`
	CumulativeReserved  money.Money `json:"cumulative_reserved"`
	CumulativeCommitted money.Money `json:"cumulative_committed"`
	Variance            money.Money `json:"variance"`
}

type pacingResp struct {
	CampaignID    string           `json:"campaign_id"`
	ConfigVersion int64            `json:"config_version"`
	Timezone      string           `json:"timezone"`
	DayKey        string           `json:"day_key"`
	DailyCap      money.Money      `json:"daily_cap"`
	PacingLimited bool             `json:"pacing_limited"`
	Slots         []slotPacingResp `json:"slots"`
}

type errorResp struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	Level          string `json:"level,omitempty"`
	DayKey         string `json:"day_key,omitempty"`
	Requested      string `json:"requested,omitempty"`
	Available      string `json:"available,omitempty"`
	CurrentVersion int64  `json:"current_version,omitempty"`
	Limit          string `json:"limit,omitempty"`
	Committed      string `json:"committed,omitempty"`
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
		Name:         req.Name,
		TotalBudget:  total,
		DailyCap:     daily,
		Timezone:     req.Timezone,
		DefaultTTL:   req.DefaultTTL.std(),
		CurveWeights: req.CurveWeights,
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
		ConfigVersion:  b.Version,
		TotalBudget:    b.TotalBudget,
		TotalSpent:     b.TotalSpent,
		TotalReserved:  b.TotalReserved,
		TotalAvailable: b.TotalAvailable,
		DayKey:         b.DayKey,
		Slot:           b.Slot,
		PacingLimited:  b.PacingLimited,
		DailyCap:       b.DailyCap,
		DailySpent:     b.DailySpent,
		DailyReserved:  b.DailyReserved,
		DailyAvailable: b.DailyAvailable,
		SlotTarget:     b.SlotTarget,
		SlotAvailable:  b.SlotAvailable,
	})
}

func (s *Server) adjustBudget(w http.ResponseWriter, r *http.Request) {
	var req adjustBudgetReq
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
	c, err := s.svc.AdjustBudget(r.Context(), domain.AdjustBudgetParams{
		CampaignID:      r.PathValue("id"),
		AdjustmentID:    req.AdjustmentID,
		ExpectedVersion: req.ExpectedVersion,
		TotalBudget:     total,
		DailyCap:        daily,
	})
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCampaignResp(c))
}

// adjustConfigRaw 用于区分 curve_weights 的三种形态：
// 字段缺省（RawMessage 为 nil）= 不变；null = 移除曲线；数组 = 替换曲线。
type adjustConfigRaw struct {
	AdjustmentID    string          `json:"adjustment_id"`
	ExpectedVersion int64           `json:"expected_version"`
	Timezone        *string         `json:"timezone"`
	CurveWeights    json.RawMessage `json:"curve_weights"`
	RemoveCurve     bool            `json:"remove_curve"`
}

func (s *Server) adjustConfig(w http.ResponseWriter, r *http.Request) {
	var raw adjustConfigRaw
	if !decode(w, r, &raw) {
		return
	}
	params := domain.AdjustConfigParams{
		CampaignID:      r.PathValue("id"),
		AdjustmentID:    raw.AdjustmentID,
		ExpectedVersion: raw.ExpectedVersion,
		Timezone:        raw.Timezone,
		RemoveCurve:     raw.RemoveCurve,
	}
	if raw.CurveWeights != nil {
		// 字段存在：null 表示移除节奏限制。
		if string(raw.CurveWeights) == "null" {
			params.RemoveCurve = true
		} else {
			var weights []int64
			if err := json.Unmarshal(raw.CurveWeights, &weights); err != nil {
				writeDomainErr(w, invalid("curve_weights must be an array of 24 integers: %v", err))
				return
			}
			params.CurveWeights = weights
			params.RemoveCurve = false
		}
	}
	c, err := s.svc.AdjustConfig(r.Context(), params)
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCampaignResp(c))
}

func (s *Server) getVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := s.svc.GetBudgetVersions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	out := make([]versionResp, len(versions))
	for i, v := range versions {
		vr := versionResp{
			Version:      v.Version,
			Kind:         v.Kind,
			AdjustmentID: v.AdjustmentID,
			TotalBudget:  v.TotalBudget,
			DailyCap:     v.DailyCap,
			Timezone:     v.Timezone,
			At:           v.At,
		}
		if v.CurveWeights != nil {
			vr.CurveWeights = v.CurveWeights
		}
		out[i] = vr
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": out})
}

func (s *Server) getPacing(w http.ResponseWriter, r *http.Request) {
	report, err := s.svc.GetPacing(r.Context(), r.PathValue("id"), r.URL.Query().Get("day"))
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	slots := make([]slotPacingResp, len(report.Slots))
	for i, sp := range report.Slots {
		slots[i] = slotPacingResp{
			Slot:                sp.Slot,
			Weight:              sp.Weight,
			CumulativeTarget:    sp.CumulativeTarget,
			SlotSpent:           sp.SlotSpent,
			SlotReserved:        sp.SlotReserved,
			CumulativeSpent:     sp.CumulativeSpent,
			CumulativeReserved:  sp.CumulativeReserved,
			CumulativeCommitted: sp.CumulativeCommitted,
			Variance:            sp.Variance,
		}
	}
	writeJSON(w, http.StatusOK, pacingResp{
		CampaignID:    report.CampaignID,
		ConfigVersion: report.Version,
		Timezone:      report.Timezone,
		DayKey:        report.DayKey,
		DailyCap:      report.DailyCap,
		PacingLimited: report.PacingLimited,
		Slots:         slots,
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
	case domain.CodeConflict, domain.CodeIdempotencyConflict, domain.CodeVersionConflict:
		status = http.StatusConflict
	}
	body := errorBody{
		Code:           string(de.Code),
		Message:        de.Message,
		Level:          de.Level,
		DayKey:         de.DayKey,
		CurrentVersion: de.CurrentVersion,
	}
	if de.Code == domain.CodeBudgetExceeded {
		body.Requested = de.Requested.String()
		body.Available = de.Available.String()
		body.Limit = de.Limit.String()
		body.Committed = de.Committed.String()
	}
	writeJSON(w, status, errorResp{Error: body})
}
