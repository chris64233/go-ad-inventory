package adinventory

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *Service, *mockClock) {
	t.Helper()
	svc, _, clock := newTestService(t, time.Minute)
	return NewServer(svc), svc, clock
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

func errCode(t *testing.T, body map[string]any) string {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %v", body)
	}
	return e["code"].(string)
}

func TestHTTPEndToEnd(t *testing.T) {
	h, _, _ := newTestServer(t)

	st, body := doJSON(t, h, "POST", "/v1/campaigns", map[string]any{
		"id": "c1", "total_budget": "100.00", "daily_budget": "30.00",
	})
	if st != http.StatusCreated {
		t.Fatalf("create campaign status=%d body=%v", st, body)
	}

	// 预占成功。
	st, body = doJSON(t, h, "POST", "/v1/reservations", map[string]any{
		"campaign_id": "c1", "request_no": "req-1", "amount": "20.00", "payload": "p",
	})
	if st != http.StatusCreated {
		t.Fatalf("reserve status=%d body=%v", st, body)
	}
	token := body["token"].(string)
	if body["status"] != "reserved" || body["expires_at"] == nil {
		t.Fatalf("bad reservation body: %v", body)
	}

	// 幂等重放。
	st, body2 := doJSON(t, h, "POST", "/v1/reservations", map[string]any{
		"campaign_id": "c1", "request_no": "req-1", "amount": "20.00", "payload": "p",
	})
	if st != http.StatusCreated || body2["token"] != token {
		t.Fatalf("idempotency over HTTP failed: %d %v", st, body2)
	}

	// 预算不足 -> 422 + budget_exceeded。
	st, body = doJSON(t, h, "POST", "/v1/reservations", map[string]any{
		"campaign_id": "c1", "request_no": "req-2", "amount": "20.00",
	})
	if st != http.StatusUnprocessableEntity || errCode(t, body) != "budget_exceeded" {
		t.Fatalf("budget exceeded mapping wrong: %d %v", st, body)
	}

	// 幂等冲突 -> 409 + idempotency_conflict。
	st, body = doJSON(t, h, "POST", "/v1/reservations", map[string]any{
		"campaign_id": "c1", "request_no": "req-1", "amount": "21.00", "payload": "p",
	})
	if st != http.StatusConflict || errCode(t, body) != "idempotency_conflict" {
		t.Fatalf("idempotency conflict mapping wrong: %d %v", st, body)
	}

	// 核销差额。
	st, body = doJSON(t, h, "POST", "/v1/reservations/"+token+"/capture", map[string]any{
		"receipt_no": "rc-1", "actual_cost": "15.50",
	})
	if st != http.StatusOK || body["status"] != "captured" {
		t.Fatalf("capture wrong: %d %v", st, body)
	}

	// 失效/终态凭证收到回执 -> 409 + state_conflict。
	st, body = doJSON(t, h, "POST", "/v1/reservations/"+token+"/capture", map[string]any{
		"receipt_no": "rc-2", "actual_cost": "1.00",
	})
	if st != http.StatusConflict || errCode(t, body) != "state_conflict" {
		t.Fatalf("state conflict mapping wrong: %d %v", st, body)
	}

	// 迟到取消 -> state_conflict。
	st, body = doJSON(t, h, "POST", "/v1/reservations/"+token+"/cancel", nil)
	if st != http.StatusConflict || errCode(t, body) != "state_conflict" {
		t.Fatalf("late cancel mapping wrong: %d %v", st, body)
	}

	// 余额查询。
	st, body = doJSON(t, h, "GET", "/v1/campaigns/c1/balance", nil)
	if st != http.StatusOK || body["total_captured"] != "15.5" {
		t.Fatalf("balance wrong: %d %v", st, body)
	}
	if body["total_available"] != "84.5" || body["day_captured"] != "15.5" {
		t.Fatalf("balance numbers wrong: %v", body)
	}
}

func TestHTTPExpiryFlow(t *testing.T) {
	h, _, clock := newTestServer(t)
	doJSON(t, h, "POST", "/v1/campaigns", map[string]any{
		"id": "c1", "total_budget": "10", "daily_budget": "10",
	})
	_, body := doJSON(t, h, "POST", "/v1/reservations", map[string]any{
		"campaign_id": "c1", "request_no": "r", "amount": "10",
	})
	token := body["token"].(string)

	clock.advance(61 * time.Second)
	st, body := doJSON(t, h, "POST", "/v1/maintenance/sweep", nil)
	if st != http.StatusOK || body["expired"] != float64(1) {
		t.Fatalf("sweep wrong: %d %v", st, body)
	}
	st, body = doJSON(t, h, "POST", "/v1/reservations/"+token+"/capture", map[string]any{
		"receipt_no": "late", "actual_cost": "10",
	})
	if st != http.StatusConflict || errCode(t, body) != "state_conflict" {
		t.Fatalf("expired capture wrong: %d %v", st, body)
	}
}

func TestHTTPErrorMappingsAndUnknownFields(t *testing.T) {
	h, _, _ := newTestServer(t)

	// 参数错误：负预算。
	st, body := doJSON(t, h, "POST", "/v1/campaigns", map[string]any{
		"id": "x", "total_budget": "-1", "daily_budget": "1",
	})
	if st != http.StatusBadRequest || errCode(t, body) != "invalid_argument" {
		t.Fatalf("negative budget: %d %v", st, body)
	}

	// 参数错误：非法 JSON / 未知字段。
	req := httptest.NewRequest("POST", "/v1/campaigns", bytes.NewReader([]byte(`{"id":"x","total_budget":"1","daily_budget":"1","bogus":1}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", rec.Code)
	}

	// not found
	st, body = doJSON(t, h, "GET", "/v1/campaigns/nope/balance", nil)
	if st != http.StatusNotFound || errCode(t, body) != "not_found" {
		t.Fatalf("not found mapping: %d %v", st, body)
	}

	// healthz
	req = httptest.NewRequest("GET", "/healthz", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
}
