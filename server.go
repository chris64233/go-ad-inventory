package adinventory

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Server 把 Service 暴露为 HTTP/JSON 接口。
type Server struct {
	svc *Service
	mux *http.ServeMux
}

// NewServer 构造 HTTP 服务处理器。
func NewServer(svc *Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/campaigns", s.createCampaign)
	s.mux.HandleFunc("GET /v1/campaigns/{id}", s.getCampaign)
	s.mux.HandleFunc("GET /v1/campaigns/{id}/balance", s.getBalance)

	s.mux.HandleFunc("POST /v1/reservations", s.reserve)
	s.mux.HandleFunc("GET /v1/reservations/{token}", s.getReservation)
	s.mux.HandleFunc("POST /v1/reservations/{token}/capture", s.capture)
	s.mux.HandleFunc("POST /v1/reservations/{token}/cancel", s.cancel)

	s.mux.HandleFunc("POST /v1/maintenance/sweep", s.sweep)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

type createCampaignReq struct {
	ID          string `json:"id"`
	TotalBudget Money  `json:"total_budget"`
	DailyBudget Money  `json:"daily_budget"`
}

func (s *Server) createCampaign(w http.ResponseWriter, r *http.Request) {
	var req createCampaignReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	c, err := s.svc.CreateCampaign(req.ID, req.TotalBudget, req.DailyBudget)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) getCampaign(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.GetCampaign(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type reserveReq struct {
	CampaignID string `json:"campaign_id"`
	RequestNo  string `json:"request_no"`
	Amount     Money  `json:"amount"`
	Payload    string `json:"payload"`
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	var req reserveReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	res, err := s.svc.Reserve(ReserveInput{
		CampaignID: req.CampaignID,
		RequestNo:  req.RequestNo,
		Amount:     req.Amount,
		Payload:    req.Payload,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) getReservation(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.GetReservation(r.PathValue("token"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type captureReq struct {
	ReceiptNo  string `json:"receipt_no"`
	ActualCost Money  `json:"actual_cost"`
}

func (s *Server) capture(w http.ResponseWriter, r *http.Request) {
	var req captureReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	res, err := s.svc.Capture(CaptureInput{
		Token:      r.PathValue("token"),
		ReceiptNo:  req.ReceiptNo,
		ActualCost: req.ActualCost,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Cancel(r.PathValue("token"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) getBalance(w http.ResponseWriter, r *http.Request) {
	day := strings.TrimSpace(r.URL.Query().Get("day"))
	b, err := s.svc.GetBalance(r.PathValue("id"), day)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) sweep(w http.ResponseWriter, _ *http.Request) {
	n := s.svc.SweepExpired()
	writeJSON(w, http.StatusOK, map[string]int{"expired": n})
}

// ---- 编解码与错误映射 ----

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalidArgumentf("invalid JSON body: %v", err)
	}
	if dec.More() {
		return invalidArgumentf("invalid JSON body: unexpected trailing data")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errBody struct {
	Error errDetail `json:"error"`
}

type errDetail struct {
	Kind  ErrorKind `json:"kind"`
	Code  string    `json:"code"`
	Scope string    `json:"scope,omitempty"`
	Msg   string    `json:"message"`
}

func writeError(w http.ResponseWriter, err error) {
	var be *Error
	if !errors.As(err, &be) {
		writeJSON(w, http.StatusInternalServerError, errBody{Error: errDetail{
			Kind: ErrorKind("internal"), Code: "internal", Msg: err.Error(),
		}})
		return
	}
	status := http.StatusInternalServerError
	switch be.Kind {
	case KindInvalidArgument:
		status = http.StatusBadRequest
	case KindNotFound:
		status = http.StatusNotFound
	case KindBudgetExceeded:
		// 语义上是当前资源状态无法完成处理，用 422 与状态冲突的 409 区分。
		status = http.StatusUnprocessableEntity
	case KindStateConflict, KindIdempotencyConflict:
		status = http.StatusConflict
	}
	writeJSON(w, status, errBody{Error: errDetail{
		Kind:  be.Kind,
		Code:  string(be.Kind),
		Scope: be.Scope,
		Msg:   be.Msg,
	}})
}
