package httpapi

import (
	"net/http"
	"testing"
)

// TestHTTPPauseResumeLifecycle 暂停 → 预占被拒（409 campaign_paused）→ 恢复 → 可预占。
func TestHTTPPauseResumeLifecycle(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "50")

	w, out := h.do("POST", "/v1/campaigns/"+id+"/pause", map[string]any{"reason": "超投排查"})
	if w.Code != http.StatusOK {
		t.Fatalf("pause: %d %v", w.Code, out)
	}
	if out["status"] != "paused" {
		t.Fatalf("status = %v, want paused", out["status"])
	}
	// 重复暂停幂等。
	w, _ = h.do("POST", "/v1/campaigns/"+id+"/pause", map[string]any{"reason": "重复"})
	if w.Code != http.StatusOK {
		t.Fatalf("re-pause: %d", w.Code)
	}

	code, out := h.reserve(id, "req-1", "10")
	if code != http.StatusConflict {
		t.Fatalf("reserve while paused: %d %v", code, out)
	}
	errBody := out["error"].(map[string]any)
	if errBody["code"] != "campaign_paused" {
		t.Fatalf("error code = %v, want campaign_paused", errBody["code"])
	}

	// 余额视图反映暂停状态，且失败请求未留下消耗。
	w, out = h.do("GET", "/v1/campaigns/"+id+"/balance", nil)
	if w.Code != http.StatusOK || out["status"] != "paused" {
		t.Fatalf("balance: %d status=%v", w.Code, out["status"])
	}
	if out["total_reserved"].(map[string]any)["amount"] != "0" {
		t.Fatalf("reserved = %v, want 0", out["total_reserved"])
	}

	w, out = h.do("POST", "/v1/campaigns/"+id+"/resume", map[string]any{"reason": "排查完成"})
	if w.Code != http.StatusOK || out["status"] != "active" {
		t.Fatalf("resume: %d %v", w.Code, out)
	}
	// 暂停期间失败的请求号未被占用，恢复后可正常使用。
	code, out = h.reserve(id, "req-1", "10")
	if code != http.StatusCreated {
		t.Fatalf("reserve after resume: %d %v", code, out)
	}
}

// TestHTTPPauseUnknownCampaign 暂停不存在的活动返回 404。
func TestHTTPPauseUnknownCampaign(t *testing.T) {
	h := newHarness(t)
	w, out := h.do("POST", "/v1/campaigns/cmp_missing/pause", map[string]any{"reason": "x"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("pause missing: %d %v", w.Code, out)
	}
}

// TestHTTPAdjustmentReasonInVersions 预算/节奏调整的原因出现在版本流中。
func TestHTTPAdjustmentReasonInVersions(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "50")

	w, out := h.do("POST", "/v1/campaigns/"+id+"/budget-adjustments", map[string]any{
		"adjustment_id":    "adj-1",
		"expected_version": 1,
		"total_budget":     moneyLine("200"),
		"daily_cap":        moneyLine("80"),
		"reason":           "双十一加量",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("adjust: %d %v", w.Code, out)
	}

	w, out = h.do("GET", "/v1/campaigns/"+id+"/versions", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("versions: %d", w.Code)
	}
	versions := out["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	v2 := versions[1].(map[string]any)
	if v2["reason"] != "双十一加量" {
		t.Fatalf("v2 reason = %v, want 双十一加量", v2["reason"])
	}
}

// TestHTTPRequestIDConflictAcrossCampaign 同一 request_id 跨活动复用返回 409 幂等冲突。
func TestHTTPRequestIDConflictAcrossCampaign(t *testing.T) {
	h := newHarness(t)
	c1 := h.createCampaign("100", "50")
	c2 := h.createCampaign("100", "50")

	code, _ := h.reserve(c1, "req-x", "10")
	if code != http.StatusCreated {
		t.Fatalf("first reserve: %d", code)
	}
	code, out := h.reserve(c2, "req-x", "10")
	if code != http.StatusConflict {
		t.Fatalf("cross-campaign reuse: %d %v", code, out)
	}
	if out["error"].(map[string]any)["code"] != "idempotency_conflict" {
		t.Fatalf("error = %v, want idempotency_conflict", out["error"])
	}
}
