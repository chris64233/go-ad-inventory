package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chris64233/go-ad-inventory/clock"
	"github.com/chris64233/go-ad-inventory/domain"
	"github.com/chris64233/go-ad-inventory/store"
)

type testHarness struct {
	t    *testing.T
	srv  *Server
	clk  *clock.Fake
	svc  *domain.Service
	base time.Time
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	base := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	clk := clock.NewFake(base)
	svc, err := domain.NewService(clk, store.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	return &testHarness{t: t, srv: NewServer(svc), clk: clk, svc: svc, base: base}
}

func (h *testHarness) do(method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	h.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, r)
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			h.t.Fatalf("decode response %q: %v", w.Body.String(), err)
		}
	}
	return w, out
}

func (h *testHarness) createCampaign(total, daily string) string {
	h.t.Helper()
	w, out := h.do("POST", "/v1/campaigns", map[string]any{
		"name":         "test",
		"total_budget": map[string]string{"amount": total, "currency": "CNY"},
		"daily_cap":    map[string]string{"amount": daily, "currency": "CNY"},
		"timezone":     "UTC",
		"default_ttl":  "1h",
	})
	if w.Code != http.StatusCreated {
		h.t.Fatalf("create campaign: %d %v", w.Code, out)
	}
	return out["id"].(string)
}

func (h *testHarness) reserve(campaignID, reqID, amount string) (int, map[string]any) {
	w, out := h.do("POST", "/v1/campaigns/"+campaignID+"/reservations", map[string]any{
		"request_id": reqID,
		"amount":     map[string]string{"amount": amount, "currency": "CNY"},
	})
	return w.Code, out
}

func TestHTTPCreateAndGetCampaign(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "50")

	w, out := h.do("GET", "/v1/campaigns/"+id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %v", w.Code, out)
	}
	if out["id"] != id {
		t.Fatalf("id = %v", out["id"])
	}
	tb := out["total_budget"].(map[string]any)
	if tb["amount"] != "100" {
		t.Fatalf("total budget = %v", tb)
	}
}

func TestHTTPInvalidArgumentMappings(t *testing.T) {
	h := newHarness(t)

	// 非法 JSON → 400。
	r := httptest.NewRequest("POST", "/v1/campaigns", strings.NewReader("{bad"))
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", w.Code)
	}

	// 非法金额字符串 → 400。
	w, out := h.do("POST", "/v1/campaigns", map[string]any{
		"name":         "x",
		"total_budget": map[string]string{"amount": "abc", "currency": "CNY"},
		"daily_cap":    map[string]string{"amount": "50", "currency": "CNY"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad amount: %d %v", w.Code, out)
	}

	// 总预算非正 → 400。
	w, out = h.do("POST", "/v1/campaigns", map[string]any{
		"name":         "x",
		"total_budget": map[string]string{"amount": "0", "currency": "CNY"},
		"daily_cap":    map[string]string{"amount": "50", "currency": "CNY"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("zero budget: %d %v", w.Code, out)
	}
}

func TestHTTPNotFound(t *testing.T) {
	h := newHarness(t)
	w, _ := h.do("GET", "/v1/campaigns/cmp_nope", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

func TestHTTPReserveBudgetExceeded(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "50")
	h.reserve(id, "req-1", "40")

	code, out := h.reserve(id, "req-2", "20")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422", code)
	}
	body := out["error"].(map[string]any)
	if body["code"] != "budget_exceeded" || body["level"] != "daily" {
		t.Fatalf("error body = %v", body)
	}
	if body["requested"] != "20" || body["available"] != "10" {
		t.Fatalf("requested=%v available=%v", body["requested"], body["available"])
	}
}

func TestHTTPIdempotencyConflict(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "50")
	h.reserve(id, "req-1", "30")

	// 同编号同内容 → 201 原凭证。
	code, out1 := h.reserve(id, "req-1", "30")
	if code != http.StatusCreated {
		t.Fatalf("replay: %d %v", code, out1)
	}
	// 同编号不同金额 → 409 idempotency_conflict。
	code, out := h.reserve(id, "req-1", "40")
	if code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", code)
	}
	if ec := out["error"].(map[string]any)["code"]; ec != "idempotency_conflict" {
		t.Fatalf("code = %v", ec)
	}
}

func TestHTTPCaptureFlowAndConflict(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "50")
	_, out := h.reserve(id, "req-1", "30")
	rid := out["id"].(string)

	capture := func(receiptID, amount string) (*httptest.ResponseRecorder, map[string]any) {
		return h.do("POST", "/v1/reservations/"+rid+"/capture", map[string]any{
			"receipt_id": receiptID,
			"amount":     map[string]string{"amount": amount, "currency": "CNY"},
		})
	}

	// 部分核销 20，释放 10。
	w, out := capture("rcpt-1", "20")
	if w.Code != http.StatusOK {
		t.Fatalf("capture: %d %v", w.Code, out)
	}
	if out["status"] != "captured" || out["captured"].(map[string]any)["amount"] != "20" {
		t.Fatalf("capture resp = %v", out)
	}

	// 余额：spent=20, available=80。
	w, bal := h.do("GET", "/v1/campaigns/"+id+"/balance", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("balance: %d", w.Code)
	}
	if bal["total_spent"].(map[string]any)["amount"] != "20" {
		t.Fatalf("spent = %v", bal["total_spent"])
	}
	if bal["total_available"].(map[string]any)["amount"] != "80" {
		t.Fatalf("available = %v", bal["total_available"])
	}

	// 重复回执 → 幂等 200。
	if w, _ := capture("rcpt-1", "20"); w.Code != http.StatusOK {
		t.Fatalf("duplicate receipt: %d", w.Code)
	}
	// 同回执不同金额 → 409 idempotency_conflict。
	w, out = capture("rcpt-1", "25")
	if w.Code != http.StatusConflict || out["error"].(map[string]any)["code"] != "idempotency_conflict" {
		t.Fatalf("receipt conflict: %d %v", w.Code, out)
	}
	// 新回执打到已核销凭证 → 409 conflict。
	w, out = capture("rcpt-2", "5")
	if w.Code != http.StatusConflict || out["error"].(map[string]any)["code"] != "conflict" {
		t.Fatalf("second capture: %d %v", w.Code, out)
	}
}

func TestHTTPCancelThenReceiptConflict(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "50")
	_, out := h.reserve(id, "req-1", "30")
	rid := out["id"].(string)

	w, out := h.do("POST", "/v1/reservations/"+rid+"/cancel", nil)
	if w.Code != http.StatusOK || out["status"] != "cancelled" {
		t.Fatalf("cancel: %d %v", w.Code, out)
	}
	// 迟到回执 → 409。
	w, out = h.do("POST", "/v1/reservations/"+rid+"/capture", map[string]any{
		"receipt_id": "rcpt-1",
		"amount":     map[string]string{"amount": "10", "currency": "CNY"},
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("capture after cancel: %d %v", w.Code, out)
	}
}

func TestHTTPExpiredReceiptConflict(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "50")
	_, out := h.reserve(id, "req-1", "30")
	rid := out["id"].(string)

	h.clk.Advance(2 * time.Hour)
	w, out := h.do("POST", "/v1/admin/expire", nil)
	if w.Code != http.StatusOK || out["expired"] != float64(1) {
		t.Fatalf("expire: %d %v", w.Code, out)
	}

	// 失效凭证收到回执 → 明确 409 conflict。
	w, out = h.do("POST", "/v1/reservations/"+rid+"/capture", map[string]any{
		"receipt_id": "rcpt-1",
		"amount":     map[string]string{"amount": "10", "currency": "CNY"},
	})
	if w.Code != http.StatusConflict || out["error"].(map[string]any)["code"] != "conflict" {
		t.Fatalf("capture after expire: %d %v", w.Code, out)
	}

	// 查询凭证状态为 expired。
	w, out = h.do("GET", "/v1/reservations/"+rid, nil)
	if w.Code != http.StatusOK || out["status"] != "expired" {
		t.Fatalf("get reservation: %d %v", w.Code, out)
	}
}
