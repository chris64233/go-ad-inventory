package httpapi

import (
	"net/http"
	"testing"
)

// 暂停 → 预占 409 conflict；恢复 → 预占恢复成功；活动响应携带 status。
func TestHTTPPauseResumeFlow(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "50")

	w, out := h.do("GET", "/v1/campaigns/"+id, nil)
	if w.Code != http.StatusOK || out["status"] != "active" {
		t.Fatalf("initial status: %d %v", w.Code, out)
	}

	w, out = h.do("POST", "/v1/campaigns/"+id+"/pause", map[string]any{
		"reason": "预算超投，临时止损",
	})
	if w.Code != http.StatusOK || out["status"] != "paused" {
		t.Fatalf("pause: %d %v", w.Code, out)
	}

	code, out := h.reserve(id, "req-1", "10")
	if code != http.StatusConflict {
		t.Fatalf("reserve while paused: %d %v", code, out)
	}
	if out["error"].(map[string]any)["code"] != "conflict" {
		t.Fatalf("error body = %v", out)
	}

	// 空 body 的恢复请求也应可用（reason 可选）。
	w, out = h.do("POST", "/v1/campaigns/"+id+"/resume", nil)
	if w.Code != http.StatusOK || out["status"] != "active" {
		t.Fatalf("resume: %d %v", w.Code, out)
	}
	code, out = h.reserve(id, "req-1", "10")
	if code != http.StatusCreated {
		t.Fatalf("reserve after resume: %d %v", code, out)
	}
}

// 预算调整携带 reason，版本流可查询到该原因。
func TestHTTPAdjustmentReasonInVersions(t *testing.T) {
	h := newHarness(t)
	id := h.createCampaign("100", "50")

	w, out := h.do("POST", "/v1/campaigns/"+id+"/budget-adjustments", map[string]any{
		"adjustment_id":    "adj-1",
		"expected_version": 1,
		"total_budget":     map[string]string{"amount": "200", "currency": "CNY"},
		"daily_cap":        map[string]string{"amount": "80", "currency": "CNY"},
		"reason":           "双十一加投",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("adjust: %d %v", w.Code, out)
	}

	w, out = h.do("GET", "/v1/campaigns/"+id+"/versions", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("versions: %d %v", w.Code, out)
	}
	versions := out["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	v2 := versions[1].(map[string]any)
	if v2["reason"] != "双十一加投" {
		t.Fatalf("version 2 reason = %v", v2["reason"])
	}
}
