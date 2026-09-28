package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// halfDayCurveTargets 与领域测试一致：上午 50%，下午 100%。
func halfDayCurveTargets() []int64 {
	targets := make([]int64, 24)
	for h := 0; h < 12; h++ {
		targets[h] = 500000
	}
	for h := 12; h < 24; h++ {
		targets[h] = 1000000
	}
	return targets
}

func (h *testHarness) createCurvedCampaign(total, daily string, curve []int64) string {
	h.t.Helper()
	body := map[string]any{
		"name":         "pacing",
		"total_budget": map[string]string{"amount": total, "currency": "CNY"},
		"daily_cap":    map[string]string{"amount": daily, "currency": "CNY"},
		"timezone":     "UTC",
		"default_ttl":  "48h",
	}
	if curve != nil {
		body["curve"] = curve
	}
	w, out := h.do("POST", "/v1/campaigns", body)
	if w.Code != http.StatusCreated {
		h.t.Fatalf("create curved campaign: %d %v", w.Code, out)
	}
	return out["id"].(string)
}

func moneyBody(amount string) map[string]string {
	return map[string]string{"amount": amount, "currency": "CNY"}
}

func TestHTTPCreateCampaignWithCurve(t *testing.T) {
	h := newHarness(t)
	curve := halfDayCurveTargets()
	w, out := h.do("POST", "/v1/campaigns", map[string]any{
		"name":         "p",
		"total_budget": moneyBody("10000"),
		"daily_cap":    moneyBody("100"),
		"timezone":     "UTC",
		"default_ttl":  "1h",
		"curve":        curve,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %v", w.Code, out)
	}
	if out["config_version"].(float64) != 1 {
		t.Fatalf("config_version = %v", out["config_version"])
	}
	gotCurve, _ := json.Marshal(out["curve"])
	wantCurve, _ := json.Marshal(curve)
	if string(gotCurve) != string(wantCurve) {
		t.Fatalf("curve = %s, want %s", gotCurve, wantCurve)
	}

	// 非法曲线（末点不满）→ 400。
	bad := make([]int64, 24)
	w, out = h.do("POST", "/v1/campaigns", map[string]any{
		"name": "p2", "total_budget": moneyBody("10000"), "daily_cap": moneyBody("100"),
		"default_ttl": "1h", "curve": bad,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad curve: %d %v", w.Code, out)
	}
}

func TestHTTPReservationCarriesFrozenFields(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("10000", "100", halfDayCurveTargets())

	code, out := h.reserve(id, "req-1", "20")
	if code != http.StatusCreated {
		t.Fatalf("reserve: %d %v", code, out)
	}
	if out["slot"].(float64) != 10 {
		t.Fatalf("slot = %v, want 10", out["slot"])
	}
	if out["day_key"] != "2026-09-26" {
		t.Fatalf("day_key = %v", out["day_key"])
	}
	if out["config_version"].(float64) != 1 {
		t.Fatalf("config_version = %v", out["config_version"])
	}
}

func TestHTTPReservePacingLimited(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("10000", "100", halfDayCurveTargets())
	h.reserve(id, "req-1", "50") // 打满上午 50% 目标

	code, out := h.reserve(id, "req-2", "1")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422", code)
	}
	body := out["error"].(map[string]any)
	if body["code"] != "budget_exceeded" || body["level"] != "pacing" {
		t.Fatalf("error = %v", body)
	}
	if body["available"] != "0" {
		t.Fatalf("available = %v", body["available"])
	}

	// 下午 12 点目标放开到 100%。
	h.clk.Set(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	if code, _ := h.reserve(id, "req-2", "50"); code != http.StatusCreated {
		t.Fatalf("reserve after noon: %d", code)
	}
}

func TestHTTPBalanceIncludesPacing(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("10000", "100", halfDayCurveTargets())
	h.reserve(id, "req-1", "20")

	w, out := h.do("GET", "/v1/campaigns/"+id+"/balance", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("balance: %d %v", w.Code, out)
	}
	if out["pacing_enabled"] != true {
		t.Fatalf("pacing_enabled = %v", out["pacing_enabled"])
	}
	if out["slot"].(float64) != 10 {
		t.Fatalf("slot = %v", out["slot"])
	}
	if out["pacing_target"].(map[string]any)["amount"] != "50" {
		t.Fatalf("pacing_target = %v", out["pacing_target"])
	}
	if out["pacing_reserved"].(map[string]any)["amount"] != "20" {
		t.Fatalf("pacing_reserved = %v", out["pacing_reserved"])
	}
	if out["pacing_available"].(map[string]any)["amount"] != "30" {
		t.Fatalf("pacing_available = %v", out["pacing_available"])
	}
}

func TestHTTPUpdateConfig(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("10000", "100", halfDayCurveTargets())

	// 切换时区，版本 1 → 2。
	w, out := h.do("PUT", "/v1/campaigns/"+id+"/config", map[string]any{
		"expected_version": 1,
		"timezone":         "Asia/Shanghai",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("update config: %d %v", w.Code, out)
	}
	if out["config_version"].(float64) != 2 || out["timezone"] != "Asia/Shanghai" {
		t.Fatalf("campaign = %v", out)
	}

	// 旧版本号再改 → 409 version_conflict，带期望与实际版本。
	w, out = h.do("PUT", "/v1/campaigns/"+id+"/config", map[string]any{
		"expected_version": 1,
		"timezone":         "UTC",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("stale version: %d %v", w.Code, out)
	}
	body := out["error"].(map[string]any)
	if body["code"] != "version_conflict" ||
		body["expected_version"].(float64) != 1 || body["actual_version"].(float64) != 2 {
		t.Fatalf("error body = %v", body)
	}

	// 用 null 移除曲线，版本 2 → 3。
	w, out = h.do("PUT", "/v1/campaigns/"+id+"/config", map[string]any{
		"expected_version": 2,
		"curve":            nil,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("remove curve: %d %v", w.Code, out)
	}
	if out["config_version"].(float64) != 3 || out["curve"] != nil {
		t.Fatalf("curve not removed: %v", out["curve"])
	}

	// 什么都不改 → 400。
	w, out = h.do("PUT", "/v1/campaigns/"+id+"/config", map[string]any{
		"expected_version": 3,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty update: %d %v", w.Code, out)
	}
}

func TestHTTPAdjustBudget(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "1000")
	h.reserve(id, "req-1", "80")

	adjust := func(adjID string, version int64, total string) (int, map[string]any) {
		w, out := h.do("POST", "/v1/campaigns/"+id+"/budget-adjustments", map[string]any{
			"adjustment_id":    adjID,
			"expected_version": version,
			"total_budget":     moneyBody(total),
		})
		return w.Code, out
	}

	// 提高到 200，版本 1 → 2，立即生效。
	code, out := adjust("adj-1", 1, "200")
	if code != http.StatusOK {
		t.Fatalf("increase: %d %v", code, out)
	}
	if out["config_version"].(float64) != 2 || out["total_budget"].(map[string]any)["amount"] != "200" {
		t.Fatalf("adjustment = %v", out)
	}

	// 同调整号重放 → 200 幂等，版本不再递增。
	if code, out := adjust("adj-1", 1, "200"); code != http.StatusOK ||
		out["config_version"].(float64) != 2 {
		t.Fatalf("idempotent replay: %d %v", code, out)
	}

	// 同调整号不同金额 → 409 idempotency_conflict。
	if code, out := adjust("adj-1", 2, "250"); code != http.StatusConflict ||
		out["error"].(map[string]any)["code"] != "idempotency_conflict" {
		t.Fatalf("adjustment conflict: %d %v", code, out)
	}

	// 过期版本号 → 409 version_conflict。
	if code, out := adjust("adj-2", 1, "300"); code != http.StatusConflict ||
		out["error"].(map[string]any)["code"] != "version_conflict" {
		t.Fatalf("version conflict: %d %v", code, out)
	}

	// 降到低于已占用（80 仍为有效预占）→ 422 total_floor，凭证不受影响。
	code, out = adjust("adj-3", 2, "79")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("decrease floor: %d %v", code, out)
	}
	body := out["error"].(map[string]any)
	if body["level"] != "total_floor" || body["available"] != "80" {
		t.Fatalf("error body = %v", body)
	}

	// 调整历史只记录成功的一次。
	wl, list := h.do("GET", "/v1/campaigns/"+id+"/budget-adjustments", nil)
	if wl.Code != http.StatusOK {
		t.Fatalf("list: %d", wl.Code)
	}
	adjs := list["adjustments"].([]any)
	if len(adjs) != 1 {
		t.Fatalf("got %d adjustments, want 1", len(adjs))
	}
	first := adjs[0].(map[string]any)
	if first["adjustment_id"] != "adj-1" || first["config_version"].(float64) != 2 {
		t.Fatalf("first adjustment = %v", first)
	}
}

func TestHTTPPacingReport(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("10000", "100", halfDayCurveTargets())
	_, out := h.reserve(id, "req-1", "50")
	rid := out["id"].(string)

	// 核销 30：差额 20 释放回冻结桶。
	w, out := h.do("POST", "/v1/reservations/"+rid+"/capture", map[string]any{
		"receipt_id": "rcpt-1",
		"amount":     moneyBody("30"),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("capture: %d %v", w.Code, out)
	}

	// 默认查今天。
	w, out = h.do("GET", "/v1/campaigns/"+id+"/pacing", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("pacing: %d %v", w.Code, out)
	}
	if out["day_key"] != "2026-09-26" || out["config_version"].(float64) != 1 {
		t.Fatalf("report head = %v", out)
	}
	slots := out["slots"].([]any)
	if len(slots) != 24 {
		t.Fatalf("slots = %d", len(slots))
	}
	s10 := slots[10].(map[string]any)
	if s10["target"].(map[string]any)["amount"] != "50" ||
		s10["spent"].(map[string]any)["amount"] != "30" ||
		s10["reserved"].(map[string]any)["amount"] != "0" ||
		s10["variance"].(map[string]any)["amount"] != "-20" {
		t.Fatalf("slot 10 = %v", s10)
	}
	if out["total_spent"].(map[string]any)["amount"] != "30" {
		t.Fatalf("total_spent = %v", out["total_spent"])
	}

	// 指定历史日期也可查询。
	w, out = h.do("GET", "/v1/campaigns/"+id+"/pacing?day=2026-09-25", nil)
	if w.Code != http.StatusOK || out["day_key"] != "2026-09-25" {
		t.Fatalf("history day: %d %v", w.Code, out)
	}
}

// TestHTTPLateReceiptAfterTimezoneSwitch 端到端验证：时区切换后迟到回执
// 仍归入冻结日期，当日余额不增加。
func TestHTTPLateReceiptAfterTimezoneSwitch(t *testing.T) {
	h := newHarness(t)
	id := h.createCurvedCampaign("10000", "1000", halfDayCurveTargets())
	_, out := h.reserve(id, "req-1", "30")
	rid := out["id"].(string)

	// 切换时区到上海（版本 2）。
	if w, out := h.do("PUT", "/v1/campaigns/"+id+"/config", map[string]any{
		"expected_version": 1,
		"timezone":         "Asia/Shanghai",
	}); w.Code != http.StatusOK {
		t.Fatalf("config: %d %v", w.Code, out)
	}

	// UTC 09-27 17:00 = 上海 09-28 01:00；凭证 48h TTL 仍有效。
	h.clk.Set(time.Date(2026, 9, 27, 17, 0, 0, 0, time.UTC))
	w, out := h.do("POST", "/v1/reservations/"+rid+"/capture", map[string]any{
		"receipt_id": "rcpt-late",
		"amount":     moneyBody("25"),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("late capture: %d %v", w.Code, out)
	}

	w, bal := h.do("GET", "/v1/campaigns/"+id+"/balance", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("balance: %d", w.Code)
	}
	// 当前是上海 09-28，当日无费用；总额度只少了核销的 25。
	if bal["day_key"] != "2026-09-28" {
		t.Fatalf("day_key = %v", bal["day_key"])
	}
	if bal["daily_spent"].(map[string]any)["amount"] != "0" {
		t.Fatalf("daily_spent = %v, want 0", bal["daily_spent"])
	}
	if bal["total_spent"].(map[string]any)["amount"] != "25" {
		t.Fatalf("total_spent = %v, want 25", bal["total_spent"])
	}
}
