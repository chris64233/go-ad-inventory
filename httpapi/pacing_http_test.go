package httpapi

import (
	"net/http"
	"testing"
)

func uniformCurve() []int64 {
	w := make([]int64, 24)
	for i := range w {
		w[i] = 1
	}
	return w
}

// createCurvedCampaign 建带投放曲线的活动，base 时间 10:00 UTC = slot 10。
func (h *testHarness) createCurvedCampaign(total, daily string, curve []int64) string {
	h.t.Helper()
	w, out := h.do("POST", "/v1/campaigns", map[string]any{
		"name":          "paced",
		"total_budget":  map[string]string{"amount": total, "currency": "CNY"},
		"daily_cap":     map[string]string{"amount": daily, "currency": "CNY"},
		"timezone":      "UTC",
		"default_ttl":   "1h",
		"curve_weights": curve,
	})
	if w.Code != http.StatusCreated {
		h.t.Fatalf("create curved campaign: %d %v", w.Code, out)
	}
	return out["id"].(string)
}

func moneyLine(amount string) map[string]string {
	return map[string]string{"amount": amount, "currency": "CNY"}
}

func TestHTTPCampaignCurveAndVersion(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("1000", "100", uniformCurve())

	w, out := h.do("GET", "/v1/campaigns/"+id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %v", w.Code, out)
	}
	if out["config_version"].(float64) != 1 {
		t.Fatalf("version = %v", out["config_version"])
	}
	cw, ok := out["curve_weights"].([]any)
	if !ok || len(cw) != 24 {
		t.Fatalf("curve_weights = %v", out["curve_weights"])
	}
	if cw[0].(float64) != 1 {
		t.Fatalf("weight = %v", cw[0])
	}
}

func TestHTTPBadCurveRejected(t *testing.T) {
	h := newHarness(t)
	w, out := h.do("POST", "/v1/campaigns", map[string]any{
		"name":          "x",
		"total_budget":  moneyLine("1000"),
		"daily_cap":     moneyLine("100"),
		"timezone":      "UTC",
		"default_ttl":   "1h",
		"curve_weights": []int64{1, 2, 3},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400, body=%v", w.Code, out)
	}
}

func TestHTTPBalanceIncludesPacing(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("1000", "100", uniformCurve())
	h.reserve(id, "req-1", "40")

	w, out := h.do("GET", "/v1/campaigns/"+id+"/balance", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("balance: %d %v", w.Code, out)
	}
	if out["slot"].(float64) != 10 || out["pacing_limited"] != true {
		t.Fatalf("pacing fields: %v", out)
	}
	slotTarget := out["slot_target"].(map[string]any)["amount"]
	if slotTarget != "45.833334" {
		t.Fatalf("slot_target = %v", slotTarget)
	}
	slotAvail := out["slot_available"].(map[string]any)["amount"]
	if slotAvail != "5.833334" {
		t.Fatalf("slot_available = %v", slotAvail)
	}
}

func TestHTTPSlotBudgetExceeded(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("1000", "100", uniformCurve())
	h.reserve(id, "req-1", "40")
	code, out := h.reserve(id, "req-2", "10")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422", code)
	}
	body := out["error"].(map[string]any)
	if body["level"] != "slot" || body["available"] != "5.833334" {
		t.Fatalf("body = %v", body)
	}
}

func TestHTTPReservationHasSlotAndVersion(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("1000", "100", uniformCurve())
	_, out := h.reserve(id, "req-1", "10")
	if out["slot"].(float64) != 10 {
		t.Fatalf("slot = %v", out["slot"])
	}
	if out["config_version"].(float64) != 1 {
		t.Fatalf("config_version = %v", out["config_version"])
	}
}

func TestHTTPAdjustBudgetLifecycle(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "200")
	h.reserve(id, "req-1", "80")

	adjust := func(adjID string, expectedVer float64, total, daily string) (int, map[string]any) {
		w, out := h.do("POST", "/v1/campaigns/"+id+"/budget-adjustments", map[string]any{
			"adjustment_id":    adjID,
			"expected_version": expectedVer,
			"total_budget":     moneyLine(total),
			"daily_cap":        moneyLine(daily),
		})
		return w.Code, out
	}

	// 成功提高到 200/200 → v2。
	code, out := adjust("adj-1", 1, "200", "200")
	if code != http.StatusOK {
		t.Fatalf("adjust: %d %v", code, out)
	}
	if out["config_version"].(float64) != 2 {
		t.Fatalf("version = %v", out["config_version"])
	}
	if out["total_budget"].(map[string]any)["amount"] != "200" {
		t.Fatalf("total = %v", out["total_budget"])
	}

	// 幂等重放，仍返回 v2。
	code, out = adjust("adj-1", 1, "200", "200")
	if code != http.StatusOK || out["config_version"].(float64) != 2 {
		t.Fatalf("idempotent replay: %d %v", code, out)
	}

	// 同号不同内容 → 409 idempotency_conflict。
	code, out = adjust("adj-1", 1, "250", "200")
	if code != http.StatusConflict ||
		out["error"].(map[string]any)["code"] != "idempotency_conflict" {
		t.Fatalf("idem conflict: %d %v", code, out)
	}

	// 期望版本过期 → 409 version_conflict，带当前版本。
	code, out = adjust("adj-2", 1, "300", "200")
	if code != http.StatusConflict {
		t.Fatalf("version conflict: %d %v", code, out)
	}
	body := out["error"].(map[string]any)
	if body["code"] != "version_conflict" || body["current_version"].(float64) != 2 {
		t.Fatalf("body = %v", body)
	}

	// 降低到低于已占用 80 → 422，带 limit/committed，凭证不被取消。
	code, out = adjust("adj-3", 2, "50", "200")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("lower below committed: %d %v", code, out)
	}
	body = out["error"].(map[string]any)
	if body["code"] != "budget_exceeded" || body["level"] != "total" ||
		body["limit"] != "50" || body["committed"] != "80" {
		t.Fatalf("reject body = %v", body)
	}
}

func TestHTTPAdjustConfigSwitchAndRemoveCurve(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("1000", "100", uniformCurve())

	// 换成仅 slot 23 投放的曲线。
	endLoaded := make([]int64, 24)
	endLoaded[23] = 24
	w, out := h.do("POST", "/v1/campaigns/"+id+"/config-adjustments", map[string]any{
		"adjustment_id":    "cfg-1",
		"expected_version": 1,
		"curve_weights":    endLoaded,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("switch curve: %d %v", w.Code, out)
	}
	if out["config_version"].(float64) != 2 {
		t.Fatalf("version = %v", out["config_version"])
	}

	// slot 10 累计目标为 0，预占被节奏层拒绝。
	if code, body := h.reserve(id, "req-1", "1"); code != http.StatusUnprocessableEntity ||
		body["error"].(map[string]any)["level"] != "slot" {
		t.Fatalf("reserve under end-loaded curve: %d %v", code, body)
	}

	// 移除曲线 → 不再受节奏限制。
	w, out = h.do("POST", "/v1/campaigns/"+id+"/config-adjustments", map[string]any{
		"adjustment_id":    "cfg-2",
		"expected_version": 2,
		"curve_weights":    nil, // JSON null
	})
	if w.Code != http.StatusOK {
		t.Fatalf("remove curve: %d %v", w.Code, out)
	}
	if _, present := out["curve_weights"]; present {
		t.Fatalf("curve_weights should be omitted, got %v", out["curve_weights"])
	}
	if code, _ := h.reserve(id, "req-2", "100"); code != http.StatusCreated {
		t.Fatalf("reserve after removing curve: %d", code)
	}
}

func TestHTTPPacingReport(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("1000", "100", uniformCurve())
	_, out := h.reserve(id, "req-1", "40")
	rid := out["id"].(string)

	w, _ := h.do("POST", "/v1/reservations/"+rid+"/capture", map[string]any{
		"receipt_id": "rcpt-1",
		"amount":     moneyLine("30"),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("capture: %d", w.Code)
	}

	w, out = h.do("GET", "/v1/campaigns/"+id+"/pacing", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("pacing: %d %v", w.Code, out)
	}
	if out["day_key"] != "2026-09-26" || out["config_version"].(float64) != 1 {
		t.Fatalf("header = %v", out)
	}
	slots := out["slots"].([]any)
	s10 := slots[10].(map[string]any)
	if s10["slot_spent"].(map[string]any)["amount"] != "30" {
		t.Fatalf("slot10 = %v", s10)
	}
	if s10["variance"].(map[string]any)["amount"] != "-15.833334" {
		t.Fatalf("variance = %v", s10["variance"])
	}

	// 指定历史日。
	w, out = h.do("GET", "/v1/campaigns/"+id+"/pacing?day=2026-09-25", nil)
	if w.Code != http.StatusOK || out["day_key"] != "2026-09-25" {
		t.Fatalf("historical day: %d %v", w.Code, out)
	}
	// 非法日期 → 400。
	w, _ = h.do("GET", "/v1/campaigns/"+id+"/pacing?day=bad", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad day: %d", w.Code)
	}
}

func TestHTTPBudgetVersions(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("100", "100", uniformCurve())

	w, _ := h.do("POST", "/v1/campaigns/"+id+"/budget-adjustments", map[string]any{
		"adjustment_id":    "adj-1",
		"expected_version": 1,
		"total_budget":     moneyLine("200"),
		"daily_cap":        moneyLine("120"),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("adjust: %d", w.Code)
	}

	w, out := h.do("GET", "/v1/campaigns/"+id+"/versions", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("versions: %d", w.Code)
	}
	versions := out["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	v0 := versions[0].(map[string]any)
	if v0["version"].(float64) != 1 || v0["kind"] != "create" {
		t.Fatalf("v0 = %v", v0)
	}
	v1 := versions[1].(map[string]any)
	if v1["version"].(float64) != 2 || v1["kind"] != "budget" ||
		v1["adjustment_id"] != "adj-1" ||
		v1["total_budget"].(map[string]any)["amount"] != "200" {
		t.Fatalf("v1 = %v", v1)
	}
}
