// Package httpapi 把领域服务暴露为 JSON HTTP 接口。
//
// 路由（Go 1.22+ 模式路由）：
//
//	POST /v1/campaigns                          创建活动
//	GET  /v1/campaigns/{id}                     查询活动配置（含配置版本与曲线）
//	PUT  /v1/campaigns/{id}/config              切换时区/投放曲线（带期望版本）
//	POST /v1/campaigns/{id}/budget-adjustments  预算调整（调整号幂等 + 期望版本）
//	GET  /v1/campaigns/{id}/budget-adjustments  查询预算版本与调整历史
//	GET  /v1/campaigns/{id}/balance             查询总预算、当日预算与当前时段节奏余额
//	GET  /v1/campaigns/{id}/pacing              查询逐时段目标/核销/预占/偏差
//	POST /v1/campaigns/{id}/reservations        预占（幂等键 request_id）
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
	s.mux.HandleFunc("PUT /v1/campaigns/{id}/config", s.updateConfig)
	s.mux.HandleFunc("POST /v1/campaigns/{id}/budget-adjustments", s.adjustBudget)
	s.mux.HandleFunc("GET /v1/campaigns/{id}/budget-adjustments", s.listAdjustments)
	s.mux.HandleFunc("GET /v1/campaigns/{id}/balance", s.getBalance)
	s.mux.HandleFunc("GET /v1/campaigns/{id}/pacing", s.getPacing)
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
	// Curve 缺省（字段不存在）或 null 表示不做时段节奏限制；
	// 给出时必须是 24 个单调非减、末点为 1_000_000 的整数（ppm）。
	Curve []int64 `json:"curve"`
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
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	TotalBudget   money.Money `json:"total_budget"`
	DailyCap      money.Money `json:"daily_cap"`
	Timezone      string      `json:"timezone"`
	DefaultTTL    string      `json:"default_ttl"`
	ConfigVersion int64       `json:"config_version"`
	// Curve 为 null 表示该活动未配置时段节奏曲线。
	Curve     []int64   `json:"curve"`
	CreatedAt time.Time `json:"created_at"`
}

func toCampaignResp(c *domain.Campaign) campaignResp {
	resp := campaignResp{
		ID:            c.ID,
		Name:          c.Name,
		TotalBudget:   c.TotalBudget,
		DailyCap:      c.DailyCap,
		Timezone:      c.Location.String(),
		DefaultTTL:    c.DefaultTTL.String(),
		ConfigVersion: c.ConfigVersion,
		CreatedAt:     c.CreatedAt,
	}
	if c.Curve != nil {
		resp.Curve = c.Curve.Targets
	}
	return resp
}

// updateConfigReq 的 Curve 用 RawMessage 以区分三种语义：
// 字段缺省（不变）、null（移除曲线）、数组（整体替换）。
type updateConfigReq struct {
	ExpectedVersion int64           `json:"expected_version"`
	Timezone        *string         `json:"timezone"`
	Curve           json.RawMessage `json:"curve"`
}

type adjustBudgetReq struct {
	AdjustmentID    string   `json:"adjustment_id"`
	ExpectedVersion int64    `json:"expected_version"`
	TotalBudget     *moneyIn `json:"total_budget"`
	DailyCap        *moneyIn `json:"daily_cap"`
}

type adjustmentResp struct {
	AdjustmentID  string      `json:"adjustment_id"`
	CampaignID    string      `json:"campaign_id"`
	ConfigVersion int64       `json:"config_version"`
	TotalBudget   money.Money `json:"total_budget"`
	DailyCap      money.Money `json:"daily_cap"`
	At            time.Time   `json:"at"`
}

func toAdjustmentResp(a domain.BudgetAdjustment) adjustmentResp {
	return adjustmentResp{
		AdjustmentID:  a.AdjustmentID,
		CampaignID:    a.CampaignID,
		ConfigVersion: a.ConfigVersion,
		TotalBudget:   a.TotalBudget,
		DailyCap:      a.DailyCap,
		At:            a.At,
	}
}

type slotPacingResp struct {
	Hour                int         `json:"hour"`
	CumulativeTargetPPM int64       `json:"cumulative_target_ppm"`
	Target              money.Money `json:"target"`
	Spent               money.Money `json:"spent"`
	Reserved            money.Money `json:"reserved"`
	CumulativeSpent     money.Money `json:"cumulative_spent"`
	Variance            money.Money `json:"variance"`
}

type pacingResp struct {
	CampaignID    string           `json:"campaign_id"`
	ConfigVersion int64            `json:"config_version"`
	Timezone      string           `json:"timezone"`
	DayKey        string           `json:"day_key"`
	DailyCap      money.Money      `json:"daily_cap"`
	CurveEnabled  bool             `json:"curve_enabled"`
	Slots         []slotPacingResp `json:"slots"`
	TotalSpent    money.Money      `json:"total_spent"`
	TotalReserved money.Money      `json:"total_reserved"`
}

func toPacingResp(r *domain.PacingReport) pacingResp {
	out := pacingResp{
		CampaignID:    r.CampaignID,
		ConfigVersion: r.ConfigVersion,
		Timezone:      r.Timezone,
		DayKey:        r.DayKey,
		DailyCap:      r.DailyCap,
		CurveEnabled:  r.CurveEnabled,
		Slots:         make([]slotPacingResp, 0, len(r.Slots)),
		TotalSpent:    r.TotalSpent,
		TotalReserved: r.TotalReserved,
	}
	for _, s := range r.Slots {
		out.Slots = append(out.Slots, slotPacingResp{
			Hour:                s.Hour,
			CumulativeTargetPPM: s.CumulativeTargetPPM,
			Target:              s.Target,
			Spent:               s.Spent,
			Reserved:            s.Reserved,
			CumulativeSpent:     s.CumulativeSpent,
			Variance:            s.Variance,
		})
	}
	return out
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
		ConfigVersion: r.ConfigVersion,
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
	DailyCap       money.Money `json:"daily_cap"`
	DailySpent     money.Money `json:"daily_spent"`
	DailyReserved  money.Money `json:"daily_reserved"`
	DailyAvailable money.Money `json:"daily_available"`
	PacingEnabled  bool        `json:"pacing_enabled"`
	PacingTarget   money.Money `json:"pacing_target"`
	PacingSpent    money.Money `json:"pacing_spent"`
	PacingReserved money.Money `json:"pacing_reserved"`
	PacingAvail    money.Money `json:"pacing_available"`
}

type errorResp struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	Level           string `json:"level,omitempty"`
	Requested       string `json:"requested,omitempty"`
	Available       string `json:"available,omitempty"`
	ExpectedVersion int64  `json:"expected_version,omitempty"`
	ActualVersion   int64  `json:"actual_version,omitempty"`
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
	var curve *domain.PacingCurve
	if req.Curve != nil {
		curve = &domain.PacingCurve{Targets: req.Curve}
	}
	c, err := s.svc.CreateCampaign(r.Context(), domain.CreateCampaignParams{
		Name:        req.Name,
		TotalBudget: total,
		DailyCap:    daily,
		Timezone:    req.Timezone,
		DefaultTTL:  req.DefaultTTL.std(),
		Curve:       curve,
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

func (s *Server) updateConfig(w http.ResponseWriter, r *http.Request) {
	var req updateConfigReq
	if !decode(w, r, &req) {
		return
	}
	params := domain.UpdateConfigParams{
		CampaignID:      r.PathValue("id"),
		ExpectedVersion: req.ExpectedVersion,
		Timezone:        req.Timezone,
	}
	// Curve 字段：缺省 = 不变；null = 移除；数组 = 整体替换。
	if len(req.Curve) > 0 {
		params.SetCurve = true
		if string(req.Curve) != "null" {
			var targets []int64
			if err := json.Unmarshal(req.Curve, &targets); err != nil {
				writeDomainErr(w, invalid("curve: %v", err))
				return
			}
			params.Curve = &domain.PacingCurve{Targets: targets}
		}
	}
	c, err := s.svc.UpdateConfig(r.Context(), params)
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCampaignResp(c))
}

func (s *Server) adjustBudget(w http.ResponseWriter, r *http.Request) {
	var req adjustBudgetReq
	if !decode(w, r, &req) {
		return
	}
	params := domain.AdjustBudgetParams{
		CampaignID:      r.PathValue("id"),
		AdjustmentID:    req.AdjustmentID,
		ExpectedVersion: req.ExpectedVersion,
	}
	if req.TotalBudget != nil {
		m, err := req.TotalBudget.parse()
		if err != nil {
			writeDomainErr(w, invalid("total_budget: %v", err))
			return
		}
		params.SetTotal = true
		params.TotalBudget = m
	}
	if req.DailyCap != nil {
		m, err := req.DailyCap.parse()
		if err != nil {
			writeDomainErr(w, invalid("daily_cap: %v", err))
			return
		}
		params.SetDaily = true
		params.DailyCap = m
	}
	adj, err := s.svc.AdjustBudget(r.Context(), params)
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAdjustmentResp(*adj))
}

func (s *Server) listAdjustments(w http.ResponseWriter, r *http.Request) {
	adjs, err := s.svc.GetBudgetAdjustments(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	out := make([]adjustmentResp, 0, len(adjs))
	for _, a := range adjs {
		out = append(out, toAdjustmentResp(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"adjustments": out})
}

func (s *Server) getPacing(w http.ResponseWriter, r *http.Request) {
	report, err := s.svc.GetPacingReport(r.Context(), r.PathValue("id"), r.URL.Query().Get("day"))
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPacingResp(report))
}

func (s *Server) getBalance(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.GetBalance(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, balanceResp{
		CampaignID:     b.CampaignID,
		ConfigVersion:  b.ConfigVersion,
		TotalBudget:    b.TotalBudget,
		TotalSpent:     b.TotalSpent,
		TotalReserved:  b.TotalReserved,
		TotalAvailable: b.TotalAvailable,
		DayKey:         b.DayKey,
		Slot:           b.Slot,
		DailyCap:       b.DailyCap,
		DailySpent:     b.DailySpent,
		DailyReserved:  b.DailyReserved,
		DailyAvailable: b.DailyAvailable,
		PacingEnabled:  b.PacingEnabled,
		PacingTarget:   b.PacingTarget,
		PacingSpent:    b.PacingSpent,
		PacingReserved: b.PacingReserved,
		PacingAvail:    b.PacingAvailable,
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
		Code:            string(de.Code),
		Message:         de.Message,
		Level:           de.Level,
		ExpectedVersion: de.ExpectedVersion,
		ActualVersion:   de.ActualVersion,
	}
	if de.Code == domain.CodeBudgetExceeded {
		body.Requested = de.Requested.String()
		body.Available = de.Available.String()
	}
	writeJSON(w, status, errorResp{Error: body})
}
